// Package api, okuma tarafının HTTP servisidir: REST uçları, canlı harita için
// WebSocket yayını ve gömülü statik site.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/nisaguven/artci/internal/store"
)

// Querier, api'nin ihtiyaç duyduğu sorgulardır; testte sahte ile değiştirilir.
type Querier interface {
	RecentEvents(ctx context.Context, since time.Time, minMag float64, limit int) ([]store.EventView, error)
	EventDetail(ctx context.Context, id string) (*store.EventDetailView, error)
	Ping(ctx context.Context) error
}

// Sorgu parametresi sınırları. Üst sınırlar tek istekle veritabanının
// yorulmasını önler; harita zaten son 24 saati gösteriyor.
const (
	defaultHours = 24
	maxHours     = 7 * 24
	defaultLimit = 1000
	maxLimit     = 2000
	minMagLimit  = -2.0
	maxMagLimit  = 10.0
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Server, HTTP uçlarını bir araya getirir.
type Server struct {
	Store  Querier
	Static fs.FS        // gömülü web sitesi
	WS     http.Handler // /ws (Hub); nil ise uç kapalı
	// RequestTimeout: WebSocket dışındaki istekler için üst süre.
	RequestTimeout time.Duration
	Now            func() time.Time
	// TelegramBot: bildirim botunun kullanıcı adı (@ olmadan). Doluysa site
	// "Telegram'da bildirim al" bağlantısını gösterir.
	TelegramBot string
}

// Handler, tüm yönlendirmeleri ve ortak ara katmanları kurar.
func (s *Server) Handler() http.Handler {
	if s.RequestTimeout <= 0 {
		s.RequestTimeout = 10 * time.Second
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	// Zaman aşımı WebSocket'e uygulanmaz: TimeoutHandler bağlantının ele
	// geçirilmesini (hijack) desteklemez ve WS zaten uzun ömürlüdür.
	timeout := func(h http.HandlerFunc) http.Handler {
		return http.TimeoutHandler(h, s.RequestTimeout, `{"error":"istek zaman aşımına uğradı"}`)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/events", timeout(s.listEvents))
	mux.Handle("GET /api/events/{id}", timeout(s.eventDetail))
	mux.Handle("GET /api/config", timeout(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"telegram_bot": s.TelegramBot})
	}))
	mux.Handle("GET /api/", timeout(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "bilinmeyen uç")
	}))
	mux.Handle("GET /healthz", timeout(s.healthz))
	if s.WS != nil {
		mux.Handle("GET /ws", s.WS)
	}
	if s.Static != nil {
		mux.Handle("GET /", timeout(http.FileServerFS(s.Static).ServeHTTP))
	}
	return securityHeaders(mux)
}

// securityHeaders, tüm yanıtlara temel güvenlik başlıklarını ekler.
// CSP yalnızca kendi kökenimize, Leaflet CDN'ine ve OSM karolarına izin verir.
// Referrer-Policy "no-referrer" değil: OSM karo kullanım politikası Referer ister.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; "+
			"script-src 'self' https://unpkg.com; "+
			"style-src 'self' https://unpkg.com; "+
			"img-src 'self' data: https://unpkg.com https://tile.openstreetmap.org; "+
			"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hours, err := intParam(q.Get("hours"), defaultHours, 1, maxHours)
	if err != nil {
		writeError(w, http.StatusBadRequest, "hours 1 ile 168 arasında bir tamsayı olmalı")
		return
	}
	limit, err := intParam(q.Get("limit"), defaultLimit, 1, maxLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit 1 ile 2000 arasında bir tamsayı olmalı")
		return
	}
	minMag := 0.0
	if v := q.Get("min_mag"); v != "" {
		minMag, err = strconv.ParseFloat(v, 64)
		// NaN karşılaştırmaları hep false olduğu için ayrıca kontrol edilir.
		if err != nil || math.IsNaN(minMag) || minMag < minMagLimit || minMag > maxMagLimit {
			writeError(w, http.StatusBadRequest, "min_mag -2 ile 10 arasında bir sayı olmalı")
			return
		}
	}
	since := s.Now().Add(-time.Duration(hours) * time.Hour)
	evs, err := s.Store.RecentEvents(r.Context(), since, minMag, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

func (s *Server) eventDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeError(w, http.StatusBadRequest, "geçersiz olay kimliği")
		return
	}
	d, err := s.Store.EventDetail(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "olay bulunamadı")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "veritabanına erişilemiyor"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// internalError, ayrıntıyı loglar ama istemciye sızdırmaz.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeError(w, http.StatusServiceUnavailable, "istek zamanında tamamlanamadı")
		return
	}
	slog.Error("api", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "sunucu hatası")
}

func intParam(v string, def, lo, hi int) (int, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, err
	}
	if n < lo || n > hi {
		return 0, strconv.ErrRange
	}
	return n, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError, tüm hatalar için tek biçim: {"error": "..."}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
