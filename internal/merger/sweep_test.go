package merger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/match"
	"github.com/nisaguven/artci/internal/replay"
)

// simulate, kataloğu varış sırasıyla bellek içi merger'dan geçirir ve
// doğruluğu döner. Gerçek merger tek tüketicili ve sıralı olduğu için
// sonuç, aynı sırayla PostGIS'e karşı çalışan merger'la aynıdır
// (replay run ile doğrulandı; bkz. docs/MATCHING.md "Eşiklerin ayarı").
func simulate(t testing.TB, entries []replay.Entry, cfg match.Config) replay.Accuracy {
	ms := newMemStore()
	svc := New(ms, &memPub{}, cfg)
	n := 0
	svc.NewID = func() string { n++; return fmt.Sprintf("ev-%05d", n) }
	svc.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	truth := map[string]string{}
	for _, e := range entries {
		r := e.Report
		r.ReceivedAt = r.OriginTime.Add(time.Duration(e.ArriveS) * time.Second)
		if _, err := svc.Handle(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		truth[key(r.Source, r.SourceEventID)] = e.Truth
	}
	var as []replay.Assignment
	for k, mr := range ms.reports {
		as = append(as, replay.Assignment{Key: k, Truth: truth[k], Event: mr.EventID, Source: mr.R.Source})
	}
	return replay.Score(as)
}

// TunedConfigAccuracy: varsayılan eşiklerin sentetik dizideki doğruluğu bir
// alt sınırın altına düşmesin (gerileme testi). Katalog her seferinde aynı
// tohumla üretilir; dosyaya gerek yok.
func TestTunedConfigOnSyntheticSequence(t *testing.T) {
	gc := replay.DefaultGenConfig()
	entries := replay.Generate(gc)
	acc := simulate(t, entries, match.DefaultConfig())
	plan := simulate(t, entries, match.PlanConfig())
	t.Logf("ayarlı eşikler: %+v", acc)
	t.Logf("plan eşikleri:  %+v", plan)
	if acc.F1 < 0.90 || acc.Precision < 0.85 {
		t.Errorf("sentetik dizide doğruluk geriledi: %+v", acc)
	}
	if acc.F1 <= plan.F1 {
		t.Errorf("ayarlı eşikler plan eşiklerinden iyi olmalı: %.3f <= %.3f", acc.F1, plan.F1)
	}
}

// TestSweep, eşik taramasıdır; yalnızca ARTCI_SWEEP=1 ile çalışır:
//
//	ARTCI_SWEEP=1 go test ./internal/merger -run Sweep -v
func TestSweep(t *testing.T) {
	if os.Getenv("ARTCI_SWEEP") == "" {
		t.Skip("ARTCI_SWEEP=1 ile çalıştırın")
	}
	// Her eşik iki gürültü düzeyinde, üçer tohumla sınanır. Sıralama iki
	// düzeyin ortalamasına göre: yalnız varsayılan gürültüye iyi oturan
	// (sentetik veriye aşırı uyan) eşikler değil, sağlam olanlar öne çıksın.
	noises := []float64{1, 2}
	catalogs := map[float64][][]replay.Entry{}
	for _, nz := range noises {
		for seed := uint64(1); seed <= 3; seed++ {
			gc := replay.DefaultGenConfig()
			gc.Seed, gc.NoiseScale = seed, nz
			catalogs[nz] = append(catalogs[nz], replay.Generate(gc))
		}
	}
	type row struct {
		name     string
		f1       map[float64]float64
		avg      float64
		prec, rc float64 // gürültü 1'de
	}
	var rows []row
	try := func(name string, cfg match.Config) {
		r := row{name: name, f1: map[float64]float64{}}
		for _, nz := range noises {
			for _, c := range catalogs[nz] {
				a := simulate(t, c, cfg)
				k := float64(len(catalogs[nz]))
				r.f1[nz] += a.F1 / k
				if nz == 1 {
					r.prec += a.Precision / k
					r.rc += a.Recall / k
				}
			}
			r.avg += r.f1[nz] / float64(len(noises))
		}
		rows = append(rows, r)
	}
	plan := match.Config{TimeWindow: 60 * time.Second, MaxDistanceKm: 100, MagScale: 1, TimeWeight: 0.4, DistWeight: 0.4,
		MagWeight: 0.2, MinScore: 0.6, SameSourceMaxDt: 5 * time.Second, SameSourceMaxKm: 10, SameSourceMaxDMag: 0.5}
	try("plan §8 (60 sn / 100 km / 0,6)", plan)
	for _, w := range []time.Duration{5, 10, 15, 20, 30, 60} {
		for _, km := range []float64{20, 30, 50, 100} {
			for _, ms := range []float64{0.5, 0.6, 0.7} {
				for _, ss := range []time.Duration{0, 2, 5} {
					c := plan
					c.TimeWindow, c.MaxDistanceKm, c.MinScore = w*time.Second, km, ms
					c.SameSourceMaxDt = ss * time.Second
					try(fmt.Sprintf("%2d sn / %3.0f km / %.1f / aynı-kaynak %d sn", w, km, ms, ss), c)
				}
			}
		}
	}
	sort.Slice(rows[1:], func(i, j int) bool { return rows[1+i].avg > rows[1+j].avg })
	t.Logf("%-44s %9s %9s %9s %9s %9s", "eşikler", "prec(1x)", "rec(1x)", "F1 1x", "F1 2x", "ortalama")
	for i, r := range rows {
		if i > 20 {
			break
		}
		t.Logf("%-44s %9.4f %9.4f %9.4f %9.4f %9.4f", r.name, r.prec, r.rc, r.f1[1], r.f1[2], r.avg)
	}
}
