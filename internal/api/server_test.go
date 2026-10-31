package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nisaguven/artci/internal/store"
)

type fakeStore struct {
	since   time.Time
	minMag  float64
	limit   int
	events  []store.EventView
	detail  *store.EventDetailView
	err     error
	pingErr error
}

func (f *fakeStore) RecentEvents(_ context.Context, since time.Time, minMag float64, limit int) ([]store.EventView, error) {
	f.since, f.minMag, f.limit = since, minMag, limit
	return f.events, f.err
}

func (f *fakeStore) EventDetail(_ context.Context, id string) (*store.EventDetailView, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.detail == nil || f.detail.ID != id {
		return nil, store.ErrNotFound
	}
	return f.detail, nil
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

var testNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

const testID = "0b6f7a5e-8f39-4c1a-9a43-7c1d2e3f4a5b"

func newTestServer(fs *fakeStore) http.Handler {
	s := &Server{
		Store:  fs,
		Static: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Artçı</title>")}},
		Now:    func() time.Time { return testNow },
	}
	return s.Handler()
}

func do(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if ct := rec.Header().Get("Content-Type"); ct == "application/json; charset=utf-8" {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("json: %v: %s", err, rec.Body)
		}
	}
	return rec, body
}

func TestListEventsDefaultsAndParams(t *testing.T) {
	fs := &fakeStore{events: []store.EventView{{ID: testID, Magnitude: 4.1, Sources: []string{"afad"}}}}
	h := newTestServer(fs)

	rec, body := do(t, h, "/api/events")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !fs.since.Equal(testNow.Add(-24*time.Hour)) || fs.minMag != 0 || fs.limit != defaultLimit {
		t.Fatalf("varsayılanlar: %v %v %v", fs.since, fs.minMag, fs.limit)
	}
	if evs := body["events"].([]any); len(evs) != 1 {
		t.Fatalf("events: %v", body)
	}

	rec, _ = do(t, h, "/api/events?hours=48&min_mag=3.5&limit=10")
	if rec.Code != 200 || !fs.since.Equal(testNow.Add(-48*time.Hour)) || fs.minMag != 3.5 || fs.limit != 10 {
		t.Fatalf("parametreler: %d %v %v %v", rec.Code, fs.since, fs.minMag, fs.limit)
	}
}

func TestListEventsEmptyIsArray(t *testing.T) {
	h := newTestServer(&fakeStore{events: []store.EventView{}})
	rec, _ := do(t, h, "/api/events")
	if got := rec.Body.String(); got != "{\"events\":[]}\n" {
		t.Fatalf("boş liste null değil dizi olmalı: %q", got)
	}
}

func TestListEventsValidation(t *testing.T) {
	h := newTestServer(&fakeStore{})
	for _, q := range []string{
		"hours=0", "hours=169", "hours=abc", "hours=1.5",
		"min_mag=11", "min_mag=-3", "min_mag=NaN", "min_mag=x",
		"limit=0", "limit=5000",
	} {
		rec, body := do(t, h, "/api/events?"+q)
		if rec.Code != http.StatusBadRequest || body["error"] == "" {
			t.Errorf("%s: %d %v", q, rec.Code, body)
		}
	}
}

func TestEventDetail(t *testing.T) {
	d := &store.EventDetailView{EventView: store.EventView{ID: testID}, Reports: []store.ReportView{{Source: "afad"}}}
	h := newTestServer(&fakeStore{detail: d})

	rec, body := do(t, h, "/api/events/"+testID)
	if rec.Code != 200 || body["id"] != testID || len(body["reports"].([]any)) != 1 {
		t.Fatalf("detay: %d %v", rec.Code, body)
	}
	rec, body = do(t, h, "/api/events/not-a-uuid")
	if rec.Code != 400 || body["error"] == nil {
		t.Fatalf("geçersiz id: %d %v", rec.Code, body)
	}
	rec, _ = do(t, h, "/api/events/11111111-2222-3333-4444-555555555555")
	if rec.Code != 404 {
		t.Fatalf("bulunamadı: %d", rec.Code)
	}
}

func TestStoreErrorsHidden(t *testing.T) {
	h := newTestServer(&fakeStore{err: errors.New("pq: gizli ayrıntı")})
	rec, body := do(t, h, "/api/events")
	if rec.Code != 500 || body["error"] != "sunucu hatası" {
		t.Fatalf("%d %v", rec.Code, body)
	}
}

func TestHealthz(t *testing.T) {
	rec, _ := do(t, newTestServer(&fakeStore{}), "/healthz")
	if rec.Code != 200 {
		t.Fatalf("ok bekleniyordu: %d", rec.Code)
	}
	rec, _ = do(t, newTestServer(&fakeStore{pingErr: errors.New("down")}), "/healthz")
	if rec.Code != 503 {
		t.Fatalf("503 bekleniyordu: %d", rec.Code)
	}
}

func TestStaticAndHeaders(t *testing.T) {
	h := newTestServer(&fakeStore{})
	rec, _ := do(t, h, "/")
	if rec.Code != 200 || rec.Body.Len() == 0 {
		t.Fatalf("index: %d", rec.Code)
	}
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(k) == "" {
			t.Errorf("%s başlığı yok", k)
		}
	}
	rec, body := do(t, h, "/api/yok")
	if rec.Code != 404 || body["error"] == nil {
		t.Fatalf("bilinmeyen api ucu JSON 404 olmalı: %d %v", rec.Code, body)
	}
	rec, _ = do(t, h, "/ws")
	if rec.Code == http.StatusSwitchingProtocols {
		t.Fatal("WS kapalıyken /ws açılmamalı")
	}
}
