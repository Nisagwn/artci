package merger

import (
	"compress/gzip"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/match"
	"github.com/nisaguven/artci/internal/replay"
)

// Gerçek dizi: 10 Ağustos 2025 Sındırgı (Mw 6,1) depreminin ilk 7 günü, ISC
// bülteninden AFAD ve Kandilli çözümleri. Doğru cevap ISC'nin olay
// gruplamasıdır. Yeniden üretmek için:
//
//	go run ./cmd/replay isc -out testdata/replay/sindirgi-isc-2025-08.jsonl.gz
const realCatalog = "../../testdata/replay/sindirgi-isc-2025-08.jsonl.gz"

func loadReal(t testing.TB) []replay.Entry {
	t.Helper()
	f, err := os.Open(realCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := replay.ReadCatalog(zr)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// reseed, aynı gözlemleri başka bir varış sırasıyla yeniden dizer. Bültende
// yayın gecikmesi yok; sonuç tek bir sıraya bağlı kalmasın.
func reseed(entries []replay.Entry, seed uint64) []replay.Entry {
	obs := make([]replay.Observation, len(entries))
	for i, e := range entries {
		obs[i] = replay.Observation{Truth: e.Truth, Report: e.Report}
	}
	return replay.CatalogFromObservations(obs, entries[0].Report.PlaceName, seed)
}

// Ölçülen değerlerin biraz altı; bkz. docs/MATCHING.md "Gerçek dizide doğrulama".
const realF1Floor, realPrecisionFloor = 0.98, 0.985

// Varsayılan eşiklerin gerçek dizideki doğruluğu bir alt sınırın altına
// düşmesin (gerileme testi). Sınır, ölçülen değerin biraz altındadır.
func TestTunedConfigOnRealSequence(t *testing.T) {
	entries := loadReal(t)
	acc := simulate(t, entries, match.DefaultConfig())
	plan := simulate(t, entries, match.PlanConfig())
	t.Logf("ayarlı eşikler: %+v", acc)
	t.Logf("plan eşikleri:  %+v", plan)
	if acc.F1 < realF1Floor || acc.Precision < realPrecisionFloor {
		t.Errorf("gerçek dizide doğruluk geriledi: %+v", acc)
	}
}

// TestSweepReal, gerçek dizide eşik taramasıdır; yalnızca ARTCI_SWEEP=1 ile:
//
//	ARTCI_SWEEP=1 go test ./internal/merger -run SweepReal -v -timeout 30m
//
// Gerçek dizide tek simülasyon saniyeler sürer; bu yüzden iki aşamalıdır:
// önce tüm bileşimler tek varış sırasıyla, sonra en iyi 10'u iki sırayla daha
// sınanır. Sentetik sütun (gürültü 1x, üç tohum), gerçek veriye aşırı uyan
// bir seçimi fark etmek içindir.
func TestSweepReal(t *testing.T) {
	if os.Getenv("ARTCI_SWEEP") == "" {
		t.Skip("ARTCI_SWEEP=1 ile çalıştırın")
	}
	base := loadReal(t)
	real := [][]replay.Entry{reseed(base, 1), reseed(base, 2), reseed(base, 3)}
	var synth [][]replay.Entry
	for seed := uint64(1); seed <= 3; seed++ {
		gc := replay.DefaultGenConfig()
		gc.Seed = seed
		synth = append(synth, replay.Generate(gc))
	}

	type row struct {
		name              string
		cfg               match.Config
		prec, rc, f1, sf1 float64
		orders            int
	}
	rows := []*row{
		{name: "plan §8 (60 sn / 100 km / 0,6)", cfg: match.PlanConfig()},
		{name: "varsayılan (10 sn / 50 km / 0,5)", cfg: match.DefaultConfig()},
	}
	for _, w := range []time.Duration{3, 5, 10, 15, 20, 30} {
		for _, km := range []float64{15, 20, 30, 50} {
			for _, ms := range []float64{0.4, 0.5, 0.6} {
				c := match.DefaultConfig()
				c.TimeWindow, c.MaxDistanceKm, c.MinScore = w*time.Second, km, ms
				rows = append(rows, &row{name: fmt.Sprintf("%2d sn / %3.0f km / %.1f", w, km, ms), cfg: c})
			}
		}
	}

	// parallel, işleri işlemci sayısı kadar goroutine'e dağıtır.
	parallel := func(jobs []func()) {
		ch := make(chan func())
		var wg sync.WaitGroup
		for range runtime.NumCPU() {
			wg.Go(func() {
				for j := range ch {
					j()
				}
			})
		}
		for _, j := range jobs {
			ch <- j
		}
		close(ch)
		wg.Wait()
	}
	var mu sync.Mutex
	runReal := func(r *row, order int) func() {
		return func() {
			a := simulate(t, real[order], r.cfg)
			mu.Lock()
			defer mu.Unlock()
			n := float64(r.orders)
			r.prec = (r.prec*n + a.Precision) / (n + 1)
			r.rc = (r.rc*n + a.Recall) / (n + 1)
			r.f1 = (r.f1*n + a.F1) / (n + 1)
			r.orders++
		}
	}

	var jobs []func()
	for _, r := range rows {
		jobs = append(jobs, runReal(r, 0))
		for _, c := range synth {
			jobs = append(jobs, func() {
				f1 := simulate(t, c, r.cfg).F1
				mu.Lock()
				r.sf1 += f1 / float64(len(synth))
				mu.Unlock()
			})
		}
	}
	parallel(jobs)

	sort.SliceStable(rows[2:], func(i, j int) bool { return rows[2+i].f1 > rows[2+j].f1 })
	jobs = nil
	for i, r := range rows {
		if i < 12 { // plan, varsayılan ve en iyi 10
			jobs = append(jobs, runReal(r, 1), runReal(r, 2))
		}
	}
	parallel(jobs)
	sort.SliceStable(rows[2:], func(i, j int) bool {
		a, b := rows[2+i], rows[2+j]
		if a.orders != b.orders {
			return a.orders > b.orders
		}
		return a.f1 > b.f1
	})

	t.Logf("%-34s %6s %8s %8s %9s %11s", "eşikler", "sıra", "prec", "rec", "F1 gerçek", "F1 sentetik")
	for i, r := range rows {
		if i > 16 {
			break
		}
		t.Logf("%-34s %6d %8.4f %8.4f %9.4f %11.4f", r.name, r.orders, r.prec, r.rc, r.f1, r.sf1)
	}
}
