// Package normalize, tüm kaynakların ortak rapor biçimini tanımlar.
// Her toplayıcı (afad, kandilli, emsc) kendi ham verisini bu tipe çevirir;
// hattın geri kalanı yalnızca bu tipi tanır.
package normalize

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kaynak adları. reports.source sütununda ve metrik etiketlerinde aynen kullanılır.
const (
	SourceAFAD     = "afad"
	SourceKandilli = "kandilli"
	SourceEMSC     = "emsc"
)

// SourcePriority: olayın gösterilen değerleri için öncelik (küçük olan kazanır).
// Plan §8: AFAD > Kandilli > EMSC.
var SourcePriority = map[string]int{
	SourceAFAD:     0,
	SourceKandilli: 1,
	SourceEMSC:     2,
}

// Report, bir kaynağın tek bir deprem kaydının ortak biçimidir.
type Report struct {
	Source        string          `json:"source"`
	SourceEventID string          `json:"source_event_id"`
	OriginTime    time.Time       `json:"origin_time"` // her zaman UTC
	Lat           float64         `json:"lat"`
	Lon           float64         `json:"lon"`
	DepthKm       *float64        `json:"depth_km,omitempty"`
	Magnitude     float64         `json:"magnitude"`
	MagType       string          `json:"mag_type,omitempty"`
	PlaceName     string          `json:"place_name,omitempty"`
	Raw           json.RawMessage `json:"raw"`
	ReceivedAt    time.Time       `json:"received_at"`
}

// Validate, açıkça bozuk kayıtları yakalar. Hata dönerse kayıt DLQ'ya gider.
func (r Report) Validate() error {
	var errs []error
	if _, ok := SourcePriority[r.Source]; !ok {
		errs = append(errs, fmt.Errorf("bilinmeyen kaynak %q", r.Source))
	}
	if strings.TrimSpace(r.SourceEventID) == "" {
		errs = append(errs, errors.New("source_event_id boş"))
	}
	if r.OriginTime.IsZero() {
		errs = append(errs, errors.New("origin_time boş"))
	}
	if r.Lat < -90 || r.Lat > 90 || r.Lon < -180 || r.Lon > 180 {
		errs = append(errs, fmt.Errorf("geçersiz koordinat %.4f,%.4f", r.Lat, r.Lon))
	}
	if r.Magnitude < -2 || r.Magnitude > 10 {
		errs = append(errs, fmt.Errorf("geçersiz büyüklük %.1f", r.Magnitude))
	}
	if r.DepthKm != nil && (*r.DepthKm < -10 || *r.DepthKm > 800) {
		errs = append(errs, fmt.Errorf("geçersiz derinlik %.1f", *r.DepthKm))
	}
	return errors.Join(errs...)
}

// Türkiye ve yakın çevresi. EMSC gibi dünya geneli kaynaklar bununla filtrelenir.
const (
	BBoxMinLat = 34.0
	BBoxMaxLat = 43.5
	BBoxMinLon = 24.0
	BBoxMaxLon = 46.0
)

// InTurkeyRegion, raporun ilgilendiğimiz bölgede olup olmadığını söyler.
func (r Report) InTurkeyRegion() bool {
	return r.Lat >= BBoxMinLat && r.Lat <= BBoxMaxLat && r.Lon >= BBoxMinLon && r.Lon <= BBoxMaxLon
}
