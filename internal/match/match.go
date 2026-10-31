// Package match, tekilleştirmenin saf (veritabanı bilmeyen) çekirdeğidir.
//
// Neden saf? Plan §8'deki "zor durumlar" tablosunu milisaniyeler içinde,
// Docker'sız birim testleriyle doğrulayabilmek için. Veritabanı ve akış işini
// merger paketi yapar; karar burada verilir.
package match

import (
	"math"
	"sort"
	"time"

	"github.com/nisaguven/artci/internal/geo"
	"github.com/nisaguven/artci/internal/normalize"
)

// Config, eşleştirme eşikleridir. Değerler Plan §8'deki başlangıç tahminleridir;
// replay ile ölçülüp ayarlanacakları için sabit değil, struct alanıdır.
type Config struct {
	// Aday penceresi: bunun dışındaki olaylar hiç aday sayılmaz.
	TimeWindow    time.Duration // ±60 sn
	MaxDistanceKm float64       // 100 km
	MagScale      float64       // |ΔM| bu değere bölünür (1,0)

	// Puan ağırlıkları (toplamı 1 olmalı ki puan 0..1 aralığında kalsın).
	TimeWeight, DistWeight, MagWeight float64

	// Bağlama eşiği: en iyi aday bu puanın altındaysa yeni olay açılır.
	MinScore float64

	// Aynı kaynaktan ikinci bir rapor (farklı id) normalde ayrı bir depremdir
	// (artçı dizisi). Ama kaynaklar bazen aynı depremi iki id ile yayınlar.
	// İkisini ayırt etmek için çok daha sıkı bir "neredeyse aynı" sınırı kullanırız.
	SameSourceMaxDt   time.Duration // 5 sn
	SameSourceMaxKm   float64       // 10 km
	SameSourceMaxDMag float64       // 0,5
}

// PlanConfig, Plan §8'deki başlangıç tahminleridir. Formül testleri bu
// değerlerle yazılmıştır; üretimde DefaultConfig kullanılır.
func PlanConfig() Config {
	return Config{
		TimeWindow:        60 * time.Second,
		MaxDistanceKm:     100,
		MagScale:          1.0,
		TimeWeight:        0.4,
		DistWeight:        0.4,
		MagWeight:         0.2,
		MinScore:          0.6,
		SameSourceMaxDt:   5 * time.Second,
		SameSourceMaxKm:   10,
		SameSourceMaxDMag: 0.5,
	}
}

// DefaultConfig, replay ile ayarlanmış eşiklerdir (bkz. docs/MATCHING.md
// "Eşiklerin ayarı"). Plan §8'in 60 sn / 100 km penceresi yoğun bir artçı
// dizisinde farklı depremleri birleştiriyordu (sentetik dizide F1 0,85);
// 10 sn / 50 km / 0,5 iki gürültü düzeyinde de ~0,95 veriyor.
// Ayar sentetik veriyle yapıldı; gerçek katalogla yeniden doğrulanmalı.
func DefaultConfig() Config {
	c := PlanConfig()
	c.TimeWindow = 10 * time.Second
	c.MaxDistanceKm = 50
	c.MinScore = 0.5
	c.SameSourceMaxDt = 2 * time.Second
	return c
}

// Member, aday olaya zaten bağlı bir raporun eşleştirme için gereken özetidir.
type Member struct {
	Source        string
	SourceEventID string
	OriginTime    time.Time
	Lat, Lon      float64
	Magnitude     float64
}

// Candidate, yeni raporun bağlanabileceği mevcut bir olaydır.
// Puan, olayın *gösterilen* değerlerine göre hesaplanır; Members ise
// "bu kaynaktan başka rapor var mı?" kuralı için gereklidir.
type Candidate struct {
	EventID    string
	OriginTime time.Time
	Lat, Lon   float64
	Magnitude  float64
	Members    []Member
}

// Score, Plan §8 formülüdür: 1 − (|Δt|/60s×0,4 + mesafe/100km×0,4 + |ΔM|/1,0×0,2).
// 1 = birebir aynı, 0 ve altı = hiç benzemiyor.
func (c Config) Score(cand Candidate, r normalize.Report) float64 {
	dt, km, dm := diffs(cand.OriginTime, cand.Lat, cand.Lon, cand.Magnitude, r)
	return 1 - (dt.Seconds()/c.TimeWindow.Seconds()*c.TimeWeight +
		km/c.MaxDistanceKm*c.DistWeight +
		dm/c.MagScale*c.MagWeight)
}

func diffs(t time.Time, lat, lon, mag float64, r normalize.Report) (time.Duration, float64, float64) {
	dt := r.OriginTime.Sub(t)
	if dt < 0 {
		dt = -dt
	}
	return dt, geo.HaversineKm(lat, lon, r.Lat, r.Lon), math.Abs(r.Magnitude - mag)
}

// inWindow: puan ne olursa olsun pencere dışı aday reddedilir. Formül tek başına
// bunu garanti etmez (ör. Δt=70 sn ama mesafe 0 ise puan hâlâ ~0,53).
func (c Config) inWindow(cand Candidate, r normalize.Report) bool {
	dt, km, _ := diffs(cand.OriginTime, cand.Lat, cand.Lon, cand.Magnitude, r)
	return dt <= c.TimeWindow && km <= c.MaxDistanceKm
}

// sameSourceOK, Plan §8 kural 4'ün ikinci yarısıdır: aday olayda bu kaynaktan
// başka rapor varsa bağlama yapılmaz — meğer ki o rapor bununla neredeyse
// aynı olsun (aynı depremin iki id ile yayınlanması).
func (c Config) sameSourceOK(cand Candidate, r normalize.Report) bool {
	for _, m := range cand.Members {
		if m.Source != r.Source || m.SourceEventID == r.SourceEventID {
			continue
		}
		dt, km, dm := diffs(m.OriginTime, m.Lat, m.Lon, m.Magnitude, r)
		if dt > c.SameSourceMaxDt || km > c.SameSourceMaxKm || dm > c.SameSourceMaxDMag {
			return false
		}
	}
	return true
}

// Action, eşleştirme kararının türüdür.
type Action int

const (
	NewEvent Action = iota // yeni olay aç
	Attach                 // mevcut olaya bağla
)

func (a Action) String() string {
	if a == Attach {
		return "attach"
	}
	return "new_event"
}

// Decision, Decide'ın sonucudur.
type Decision struct {
	Action  Action
	EventID string  // Attach ise hedef olay
	Score   float64 // Attach ise kazanan puan (log/metrik için)
}

// Decide, raporun hangi olaya ait olduğuna karar verir (Plan §8 adım 3-5).
// Eşit puanda EventID'ye göre seçilir ki sonuç aday sırasına bağlı olmasın.
func (c Config) Decide(r normalize.Report, cands []Candidate) Decision {
	best := Decision{Action: NewEvent}
	for _, cand := range cands {
		if !c.inWindow(cand, r) || !c.sameSourceOK(cand, r) {
			continue
		}
		s := c.Score(cand, r)
		if s < c.MinScore {
			continue
		}
		if best.Action == NewEvent || s > best.Score || (s == best.Score && cand.EventID < best.EventID) {
			best = Decision{Action: Attach, EventID: cand.EventID, Score: s}
		}
	}
	return best
}

// MemberOf, bir raporu Member'a çevirir.
func MemberOf(r normalize.Report) Member {
	return Member{Source: r.Source, SourceEventID: r.SourceEventID, OriginTime: r.OriginTime,
		Lat: r.Lat, Lon: r.Lon, Magnitude: r.Magnitude}
}

// CandidateOf, olayın raporlarından bir aday üretir. Güncellenen bir raporun
// hâlâ olayına uyup uymadığını sınarken kullanılır (raporun kendisi hariç).
func CandidateOf(eventID string, reports []normalize.Report) Candidate {
	ev := Resolve(reports)
	cand := Candidate{EventID: eventID, OriginTime: ev.OriginTime, Lat: ev.Lat, Lon: ev.Lon, Magnitude: ev.Magnitude}
	for _, r := range reports {
		cand.Members = append(cand.Members, MemberOf(r))
	}
	return cand
}

// Resolve, olayın gösterilen değerlerini raporlarından hesaplar (Plan §8 kaynak
// önceliği): AFAD > Kandilli > EMSC; eşit öncelikte en son alınan rapor kazanır
// (kaynak kendi değerini düzeltmiş olabilir). Sonuç yalnızca rapor *kümesine*
// bağlıdır, geliş sırasına değil — sıra bağımsızlığının temeli budur.
//
// ID, Version, CreatedAt, UpdatedAt doldurulmaz; onlar depolamanın işidir.
func Resolve(reports []normalize.Report) normalize.Event {
	if len(reports) == 0 {
		return normalize.Event{}
	}
	rs := append([]normalize.Report(nil), reports...)
	sort.Slice(rs, func(i, j int) bool {
		pi, pj := priority(rs[i].Source), priority(rs[j].Source)
		if pi != pj {
			return pi < pj
		}
		if !rs[i].ReceivedAt.Equal(rs[j].ReceivedAt) {
			return rs[i].ReceivedAt.After(rs[j].ReceivedAt)
		}
		return rs[i].SourceEventID < rs[j].SourceEventID // tam eşitlikte deterministik
	})
	w := rs[0]
	ev := normalize.Event{
		OriginTime: w.OriginTime.UTC(),
		Lat:        w.Lat,
		Lon:        w.Lon,
		DepthKm:    w.DepthKm,
		Magnitude:  w.Magnitude,
		MagType:    w.MagType,
		PlaceName:  w.PlaceName,
	}
	seen := map[string]bool{}
	for _, r := range rs { // rs önceliğe göre sıralı -> Sources da öyle
		if !seen[r.Source] {
			seen[r.Source] = true
			ev.Sources = append(ev.Sources, r.Source)
		}
		if ev.FirstReportedAt.IsZero() || r.ReceivedAt.Before(ev.FirstReportedAt) {
			ev.FirstReportedAt = r.ReceivedAt.UTC()
		}
	}
	return ev
}

func priority(source string) int {
	if p, ok := normalize.SourcePriority[source]; ok {
		return p
	}
	return math.MaxInt // bilinmeyen kaynak en sona
}
