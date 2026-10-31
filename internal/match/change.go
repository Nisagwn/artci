package match

import (
	"math"
	"slices"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

// Veritabanı büyüklük ve derinliği NUMERIC(…,1) olarak saklar. Karşılaştırmalar
// da aynı hassasiyette yapılmalı; yoksa 4.25 ile 4.3 "farklı" görünür ve her
// yeniden okumada boş yere sürüm artar.
func round1(x float64) float64 { return math.Round(x*10) / 10 }

// sameTime: PostgreSQL mikro saniye saklar, Go nano saniye taşır. Gidiş-dönüşte
// oluşan alt-mikro saniye farkı değişiklik sayılmamalı.
func sameTime(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -time.Microsecond && d < time.Microsecond
}

// coordEps: ~1 cm. Kayan nokta gidiş-dönüşündeki gürültüyü değişiklik saymamak için.
const coordEps = 1e-7

func sameDepth(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return round1(*a) == round1(*b)
}

// ReportChanged, aynı (source, source_event_id) raporunun yeni hâlinin anlamca
// farklı olup olmadığını söyler. ReceivedAt ve Raw bilerek dışarıda: polling her
// turda aynı kaydı yeni bir ReceivedAt ile getirir; bunu güncelleme saymak
// idempotency'yi bozardı.
func ReportChanged(prev, next normalize.Report) bool {
	return !sameTime(prev.OriginTime, next.OriginTime) ||
		math.Abs(prev.Lat-next.Lat) > coordEps ||
		math.Abs(prev.Lon-next.Lon) > coordEps ||
		!sameDepth(prev.DepthKm, next.DepthKm) ||
		round1(prev.Magnitude) != round1(next.Magnitude) ||
		prev.MagType != next.MagType ||
		prev.PlaceName != next.PlaceName
}

// EventChanged, olayın dışarıya gösterilen (events.updated'a yazılan) değerlerinde
// değişiklik var mı söyler. true ise version artar ve olay yayınlanır.
// Kaynak listesi de dahildir: haritada "Kaynaklar: AFAD, Kandilli" değişir.
//
// Not: Bu, "kullanıcıya yeniden bildirim gitmeli mi?" sorusu değildir; o eşik
// (ΔM ≥ 0,5) notifier'dadır ve PrevMagnitude ile hesaplanır (Plan §10).
func EventChanged(prev, next normalize.Event) bool {
	return !sameTime(prev.OriginTime, next.OriginTime) ||
		math.Abs(prev.Lat-next.Lat) > coordEps ||
		math.Abs(prev.Lon-next.Lon) > coordEps ||
		!sameDepth(prev.DepthKm, next.DepthKm) ||
		round1(prev.Magnitude) != round1(next.Magnitude) ||
		prev.MagType != next.MagType ||
		prev.PlaceName != next.PlaceName ||
		!slices.Equal(prev.Sources, next.Sources) ||
		!sameTime(prev.FirstReportedAt, next.FirstReportedAt)
}
