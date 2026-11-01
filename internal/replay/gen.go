package replay

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

// GenConfig, sentetik artçı dizisinin parametreleridir.
//
// Sentetik dizi, gerçek bir kataloğun yerine geçmez; ama her raporun hangi
// gerçek depreme ait olduğunu kesin bildiği için birleştirme doğruluğunu
// ölçmeyi mümkün kılar. Gerçek bir katalog (ör. AFAD + Kandilli + EMSC'nin
// aynı dönem kayıtları) elde edildiğinde aynı oynatıcı onunla da çalışır,
// ancak o zaman "truth" etiketleri elle ya da katalog eşlemesiyle verilmelidir.
type GenConfig struct {
	Seed     uint64
	Start    time.Time // ana şokun oluş zamanı
	Lat, Lon float64   // ana şok merkezi
	MainMag  float64
	Duration time.Duration // dizinin süresi
	Count    int           // artçı sayısı (ana şok hariç)
	MinMag   float64       // tamlık eşiği
	BValue   float64       // Gutenberg-Richter b değeri
	OmoriC   time.Duration // Omori c (erken dönem doygunluğu)
	OmoriP   float64       // Omori p
	SpreadKm float64       // artçıların merkezden saçılımı (1σ)
	// EarlyIncomplete: ana şoktan sonraki bu ölçekte M<3 depremlerin
	// kaynaklarca kaçırılma olasılığı yüksektir (p *= 1 - e^(-t/ölçek)).
	EarlyIncomplete time.Duration
	// NoiseScale: kaynaklar arası ölçüm farklarının (zaman, konum, büyüklük)
	// çarpanı. 1 = varsayılan model; 2 ile eşiklerin sağlamlığı sınanır.
	NoiseScale float64
	PlaceName  string
}

// DefaultGenConfig: Sındırgı benzeri M6,1 ana şok ve 24 saatte 400 artçı.
//
// Yoğunluk bilinçli olarak zorlayıcıdır: ilk saatte ~3 deprem/dk, ilk
// 10 dakikada ~7 deprem/dk. Kamuya açık bilgiye göre gerçek dizide ilk gün
// "yüzlerce" artçı kaydedildi; buradaki sayı o mertebenin üst ucundadır.
// Ana şoktan hemen sonra küçük depremlerin kataloglarda eksik kalması
// (kısa vadeli eksiklik) EarlyIncomplete ile modellenir.
func DefaultGenConfig() GenConfig {
	return GenConfig{
		Seed: 1, Start: time.Date(2026, 8, 10, 16, 53, 0, 0, time.UTC),
		Lat: 39.21, Lon: 28.15, MainMag: 6.1,
		Duration: 24 * time.Hour, Count: 400, MinMag: 1.5, BValue: 1.0,
		OmoriC: 10 * time.Minute, OmoriP: 1.1, SpreadKm: 8,
		EarlyIncomplete: 20 * time.Minute,
		PlaceName:       "Sındırgı (Balıkesir)",
	}
}

// sourceModel: bir kaynağın tamlığı, gecikmesi ve ölçüm farkları.
type sourceModel struct {
	name                   string
	detectAbove            float64 // bu büyüklüğün altında raporlama olasılığı düşer
	detectP                float64 // eşik üstünde raporlama olasılığı
	delayMin, delayMax     float64 // saniye
	dtSigma                float64 // oluş zamanı farkı (sn, 1σ)
	locSigmaKm             float64
	magSigma               float64
	magType                string
	reviseP                float64 // sonradan revize rapor yayınlama olasılığı
	reviseDelay            float64 // revizyonun ek gecikmesi (sn)
	stalledAfterBurstScale float64 // yoğunlukta gecikmenin artışı (oran)
}

var sourceModels = []sourceModel{
	{"afad", 2.0, 0.97, 90, 300, 0.8, 2.5, 0.12, "ML", 0.10, 900, 0.5},
	{"kandilli", 2.0, 0.92, 150, 480, 1.0, 3.0, 0.15, "ML", 0.15, 1200, 0.8},
	{"emsc", 2.8, 0.85, 60, 240, 1.5, 4.0, 0.20, "mb", 0.05, 600, 0.3},
}

// quake, gerçek (truth) depremdir.
type quake struct {
	id                string
	t                 time.Time
	lat, lon, depth   float64
	mag               float64
	minutesSinceStart float64
}

// Generate, sentetik diziyi üretir. Aynı tohum aynı kataloğu verir.
func Generate(c GenConfig) []Entry {
	rng := rand.New(rand.NewPCG(c.Seed, c.Seed^0x9e3779b97f4a7c15))
	quakes := []quake{{id: "T0000", t: c.Start, lat: c.Lat, lon: c.Lon, depth: 11, mag: c.MainMag}}
	for i := 1; i <= c.Count; i++ {
		dt := omoriSample(rng, c.OmoriC.Seconds(), c.OmoriP, c.Duration.Seconds())
		dLat, dLon := kmToDeg(rng.NormFloat64()*c.SpreadKm, rng.NormFloat64()*c.SpreadKm, c.Lat)
		m := grSample(rng, c.MinMag, c.BValue, c.MainMag-0.3)
		quakes = append(quakes, quake{
			id: fmt.Sprintf("T%04d", i), t: c.Start.Add(time.Duration(dt * float64(time.Second))),
			lat: c.Lat + dLat, lon: c.Lon + dLon, depth: math.Max(1, 5+rng.Float64()*15), mag: m,
			minutesSinceStart: dt / 60,
		})
	}
	sort.Slice(quakes, func(i, j int) bool { return quakes[i].t.Before(quakes[j].t) })

	var out []Entry
	for _, q := range quakes {
		for _, sm := range sourceModels {
			p := sm.detectP
			if q.mag < sm.detectAbove {
				p *= 0.35 // eşik altında yalnızca bir kısmı kaydedilir
			}
			if q.mag < 3 && c.EarlyIncomplete > 0 {
				p *= 1 - math.Exp(-q.minutesSinceStart/c.EarlyIncomplete.Minutes())
			}
			if rng.Float64() > p {
				continue
			}
			// İlk dakikalarda kaynaklar yığılır, gecikme artar.
			burst := 1 + sm.stalledAfterBurstScale*math.Exp(-q.minutesSinceStart/30)
			delay := (sm.delayMin + rng.Float64()*(sm.delayMax-sm.delayMin)) * burst
			r := observe(rng, q, sm, c.noise())
			r.SourceEventID = fmt.Sprintf("%s-%s", sm.name, q.id)
			arrive := q.t.Sub(c.Start).Seconds() + delay
			out = append(out, Entry{Truth: q.id, ArriveS: arrive, Report: r})
			if rng.Float64() < sm.reviseP {
				rev := observe(rng, q, sm, c.noise())
				rev.SourceEventID = r.SourceEventID // aynı kimlik: revizyon
				out = append(out, Entry{Truth: q.id, ArriveS: arrive + sm.reviseDelay*(0.5+rng.Float64()), Report: rev})
			}
		}
	}
	for i := range out {
		out[i].Report.PlaceName = c.PlaceName
		raw, _ := json.Marshal(map[string]any{"synthetic": true, "truth": out[i].Truth})
		out[i].Report.Raw = raw
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ArriveS < out[j].ArriveS })
	return out
}

func (c GenConfig) noise() float64 {
	if c.NoiseScale <= 0 {
		return 1
	}
	return c.NoiseScale
}

func observe(rng *rand.Rand, q quake, sm sourceModel, k float64) normalize.Report {
	sm.dtSigma, sm.locSigmaKm, sm.magSigma = sm.dtSigma*k, sm.locSigmaKm*k, sm.magSigma*k
	dLat, dLon := kmToDeg(rng.NormFloat64()*sm.locSigmaKm, rng.NormFloat64()*sm.locSigmaKm, q.lat)
	d := math.Round(math.Max(0, q.depth+rng.NormFloat64()*2)*10) / 10
	return normalize.Report{
		Source:     sm.name,
		OriginTime: q.t.Add(time.Duration(rng.NormFloat64() * sm.dtSigma * float64(time.Second))).Truncate(100 * time.Millisecond).UTC(),
		Lat:        round4(q.lat + dLat), Lon: round4(q.lon + dLon), DepthKm: &d,
		Magnitude: math.Round((q.mag+rng.NormFloat64()*sm.magSigma)*10) / 10,
		MagType:   sm.magType,
	}
}

// omoriSample: değiştirilmiş Omori yasasına göre [0, T] aralığında bir an.
// Yoğunluk n(t) ∝ (t+c)^-p; ters CDF ile örneklenir.
func omoriSample(rng *rand.Rand, c, p, T float64) float64 {
	u := rng.Float64()
	if math.Abs(p-1) < 1e-9 {
		return c*math.Pow((T+c)/c, u) - c
	}
	a := math.Pow(c, 1-p)
	b := math.Pow(T+c, 1-p)
	return math.Pow(a+u*(b-a), 1/(1-p)) - c
}

// grSample: Gutenberg-Richter dağılımından (m >= mMin) büyüklük, mMax ile kesilir.
func grSample(rng *rand.Rand, mMin, b, mMax float64) float64 {
	for {
		m := mMin - math.Log10(1-rng.Float64())/b
		if m <= mMax {
			return math.Round(m*10) / 10
		}
	}
}

func kmToDeg(northKm, eastKm, lat float64) (float64, float64) {
	return northKm / 111.32, eastKm / (111.32 * math.Cos(lat*math.Pi/180))
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }
