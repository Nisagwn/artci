package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

// AFADBaseURL: AFAD olay filtreleme API'si (apiv2).
const AFADBaseURL = "https://deprem.afad.gov.tr/apiv2/event/filter"

// afadEvent, apiv2 yanıtındaki tek kayıttır. Sayısal alanlar API'de string
// olarak gelir ("39.2100"); flexNum ikisini de kabul eder.
type afadEvent struct {
	EventID   json.RawMessage `json:"eventID"`
	Date      string          `json:"date"`
	Latitude  flexNum         `json:"latitude"`
	Longitude flexNum         `json:"longitude"`
	Depth     flexNum         `json:"depth"`
	Magnitude flexNum         `json:"magnitude"`
	Type      string          `json:"type"`
	Location  string          `json:"location"`
	Province  string          `json:"province"`
	District  string          `json:"district"`
}

// flexNum, JSON sayısını ya da sayı içeren string'i okur. Boş/null -> geçersiz.
type flexNum struct {
	V     float64
	Valid bool
}

func (f *flexNum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = flexNum{}
		return nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil {
		return fmt.Errorf("sayı değil: %q", s)
	}
	*f = flexNum{V: v, Valid: true}
	return nil
}

// afadTimeLayouts: API tarihleri UTC'dir ve saat dilimi eki taşımaz.
var afadTimeLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
}

func parseAFADTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range afadTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("tarih çözülemedi: %q", s)
}

// ParseAFAD, apiv2 yanıtını raporlara çevirir. Yanıtın kendisi JSON dizisi
// değilse hata döner (kaynak arızası); tek tek bozuk kayıtlar BadRecord olur.
func ParseAFAD(body []byte, receivedAt time.Time) (Result, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		return Result{}, fmt.Errorf("afad: yanıt JSON dizisi değil: %w", err)
	}
	var reports []normalize.Report
	var bad []BadRecord
	for _, raw := range items {
		r, err := parseAFADEvent(raw, receivedAt)
		if err != nil {
			bad = append(bad, BadRecord{Raw: string(raw), Reason: err.Error()})
			continue
		}
		reports = append(reports, r)
	}
	return finish(reports, bad), nil
}

func parseAFADEvent(raw json.RawMessage, receivedAt time.Time) (normalize.Report, error) {
	var e afadEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return normalize.Report{}, fmt.Errorf("afad: %w", err)
	}
	id := strings.Trim(strings.TrimSpace(string(e.EventID)), `"`)
	if id == "" || id == "null" {
		return normalize.Report{}, fmt.Errorf("afad: eventID yok")
	}
	t, err := parseAFADTime(e.Date)
	if err != nil {
		return normalize.Report{}, fmt.Errorf("afad: %w", err)
	}
	if !e.Latitude.Valid || !e.Longitude.Valid || !e.Magnitude.Valid {
		return normalize.Report{}, fmt.Errorf("afad: enlem/boylam/büyüklük eksik")
	}
	r := normalize.Report{
		Source:        normalize.SourceAFAD,
		SourceEventID: id,
		OriginTime:    t,
		Lat:           e.Latitude.V,
		Lon:           e.Longitude.V,
		Magnitude:     e.Magnitude.V,
		MagType:       e.Type,
		PlaceName:     afadPlace(e),
		Raw:           raw,
		ReceivedAt:    receivedAt.UTC(),
	}
	if e.Depth.Valid {
		d := e.Depth.V
		r.DepthKm = &d
	}
	return r, nil
}

func afadPlace(e afadEvent) string {
	if l := strings.TrimSpace(e.Location); l != "" {
		return l
	}
	switch {
	case e.District != "" && e.Province != "":
		return e.District + " (" + e.Province + ")"
	default:
		return e.Province
	}
}

// AFAD, AFAD API'sini sorgulayan Fetcher'dır.
type AFAD struct {
	HTTP    *HTTPClient
	BaseURL string        // boşsa AFADBaseURL
	Window  time.Duration // geriye doğru sorgu penceresi (varsayılan 1 saat)
	Now     func() time.Time
}

func (a *AFAD) Name() string { return normalize.SourceAFAD }

// URL, verilen an için sorgu adresini üretir. Bölge filtresi uygulanır;
// AFAD zaten Türkiye ağırlıklı, sınır bölgeleri için bbox yeterli.
func (a *AFAD) URL(now time.Time) string {
	base := a.BaseURL
	if base == "" {
		base = AFADBaseURL
	}
	w := a.Window
	if w == 0 {
		w = time.Hour
	}
	const l = "2006-01-02T15:04:05"
	q := url.Values{}
	q.Set("start", now.Add(-w).UTC().Format(l))
	q.Set("end", now.Add(time.Minute).UTC().Format(l))
	q.Set("minlat", fmt.Sprint(normalize.BBoxMinLat))
	q.Set("maxlat", fmt.Sprint(normalize.BBoxMaxLat))
	q.Set("minlon", fmt.Sprint(normalize.BBoxMinLon))
	q.Set("maxlon", fmt.Sprint(normalize.BBoxMaxLon))
	q.Set("orderby", "timedesc")
	q.Set("format", "json")
	return base + "?" + q.Encode()
}

func (a *AFAD) Fetch(ctx context.Context) (Result, error) {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	body, err := a.HTTP.Get(ctx, a.URL(now()))
	if err != nil {
		return Result{}, err
	}
	return ParseAFAD(body, now())
}
