package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

// EMSC SeismicPortal adresleri.
const (
	EMSCWebSocketURL = "wss://www.seismicportal.eu/standing_order/websocket"
	EMSCFDSNURL      = "https://www.seismicportal.eu/fdsnws/event/1/query"
)

// emscFeature, SeismicPortal'ın GeoJSON Feature biçimidir (hem FDSN hem WebSocket).
type emscFeature struct {
	ID         string `json:"id"`
	Properties struct {
		UNID         string  `json:"unid"`
		Time         string  `json:"time"`
		Lat          flexNum `json:"lat"`
		Lon          flexNum `json:"lon"`
		Depth        flexNum `json:"depth"`
		Mag          flexNum `json:"mag"`
		MagType      string  `json:"magtype"`
		FlynnRegion  string  `json:"flynn_region"`
		EvType       string  `json:"evtype"`
		LastUpdate   string  `json:"lastupdate"`
		SourceCatalg string  `json:"source_catalog"`
	} `json:"properties"`
}

// EMSCMessage, WebSocket akışındaki tek mesajdır: {"action":"create|update","data":Feature}.
type EMSCMessage struct {
	Action string          `json:"action"`
	Data   json.RawMessage `json:"data"`
}

// ErrOutOfRegion, bölge dışı kayıtlar için döner; bunlar bozuk değildir, sessizce atlanır.
var ErrOutOfRegion = fmt.Errorf("emsc: bölge dışı")

// ParseEMSCFeature, tek bir Feature'ı rapora çevirir.
func ParseEMSCFeature(raw json.RawMessage, receivedAt time.Time) (normalize.Report, error) {
	var f emscFeature
	if err := json.Unmarshal(raw, &f); err != nil {
		return normalize.Report{}, fmt.Errorf("emsc: %w", err)
	}
	p := f.Properties
	id := p.UNID
	if id == "" {
		id = f.ID
	}
	if id == "" {
		return normalize.Report{}, fmt.Errorf("emsc: unid yok")
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(p.Time))
	if err != nil {
		return normalize.Report{}, fmt.Errorf("emsc: tarih çözülemedi: %q", p.Time)
	}
	if !p.Lat.Valid || !p.Lon.Valid || !p.Mag.Valid {
		return normalize.Report{}, fmt.Errorf("emsc: enlem/boylam/büyüklük eksik")
	}
	r := normalize.Report{
		Source:        normalize.SourceEMSC,
		SourceEventID: id,
		OriginTime:    t.UTC(),
		Lat:           p.Lat.V,
		Lon:           p.Lon.V,
		Magnitude:     p.Mag.V,
		MagType:       p.MagType,
		PlaceName:     p.FlynnRegion,
		Raw:           raw,
		ReceivedAt:    receivedAt.UTC(),
	}
	if p.Depth.Valid {
		d := p.Depth.V
		r.DepthKm = &d
	}
	if !r.InTurkeyRegion() {
		return r, ErrOutOfRegion
	}
	return r, nil
}

// ParseEMSCMessage, WebSocket mesajını çözer. Bölge dışı ise ErrOutOfRegion döner.
func ParseEMSCMessage(msg []byte, receivedAt time.Time) (normalize.Report, error) {
	var m EMSCMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return normalize.Report{}, fmt.Errorf("emsc: mesaj çözülemedi: %w", err)
	}
	if len(m.Data) == 0 {
		return normalize.Report{}, fmt.Errorf("emsc: mesajda data yok (action=%q)", m.Action)
	}
	return ParseEMSCFeature(m.Data, receivedAt)
}

// ParseEMSCFDSN, FDSN olay servisinin GeoJSON FeatureCollection yanıtını çözer.
// Bölge dışı kayıtlar atlanır (sorgu zaten bbox'lı, bu ikinci güvence).
func ParseEMSCFDSN(body []byte, receivedAt time.Time) (Result, error) {
	var fc struct {
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(body, &fc); err != nil {
		return Result{}, fmt.Errorf("emsc fdsn: %w", err)
	}
	var reports []normalize.Report
	var bad []BadRecord
	for _, raw := range fc.Features {
		r, err := ParseEMSCFeature(raw, receivedAt)
		switch {
		case err == ErrOutOfRegion:
		case err != nil:
			bad = append(bad, BadRecord{Raw: string(raw), Reason: err.Error()})
		default:
			reports = append(reports, r)
		}
	}
	return finish(reports, bad), nil
}

// EMSCFDSN, belirli bir andan bu yana bölgedeki olayları FDSN ile çeker.
// Hem kopukluk sonrası boşluk doldurmada hem de açılışta kullanılır.
type EMSCFDSN struct {
	HTTP    *HTTPClient
	BaseURL string // boşsa EMSCFDSNURL
	Now     func() time.Time
}

// URL, since anından itibaren bölge sorgusu üretir.
func (e *EMSCFDSN) URL(since time.Time) string {
	base := e.BaseURL
	if base == "" {
		base = EMSCFDSNURL
	}
	q := url.Values{}
	q.Set("format", "json")
	q.Set("start", since.UTC().Format("2006-01-02T15:04:05"))
	q.Set("minlat", fmt.Sprint(normalize.BBoxMinLat))
	q.Set("maxlat", fmt.Sprint(normalize.BBoxMaxLat))
	q.Set("minlon", fmt.Sprint(normalize.BBoxMinLon))
	q.Set("maxlon", fmt.Sprint(normalize.BBoxMaxLon))
	q.Set("orderby", "time")
	q.Set("limit", "500")
	return base + "?" + q.Encode()
}

// Since, since anından bu yana kayıtları döner. FDSN sonuç yoksa 204 döner.
func (e *EMSCFDSN) Since(ctx context.Context, since time.Time) (Result, error) {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	body, err := e.HTTP.Get(ctx, e.URL(since))
	if err != nil {
		return Result{}, err
	}
	if len(strings.TrimSpace(string(body))) == 0 { // 204 No Content
		return Result{}, nil
	}
	return ParseEMSCFDSN(body, now())
}
