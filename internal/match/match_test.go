package match

import (
	"math"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

var t0 = time.Date(2026, 10, 30, 12, 4, 31, 0, time.UTC)

func fptr(f float64) *float64 { return &f }

func rep(src, id string, dtSec, lat, lon, mag float64) normalize.Report {
	return normalize.Report{
		Source: src, SourceEventID: id,
		OriginTime: t0.Add(time.Duration(dtSec * float64(time.Second))),
		Lat:        lat, Lon: lon, Magnitude: mag, DepthKm: fptr(10),
		ReceivedAt: t0.Add(time.Minute),
	}
}

func candOf(id string, rs ...normalize.Report) Candidate { return CandidateOf(id, rs) }

func TestScore(t *testing.T) {
	c := PlanConfig() // formül Plan §8 değerleriyle sınanır
	base := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	cand := candOf("E1", base)

	tests := []struct {
		name string
		r    normalize.Report
		want float64
	}{
		{"birebir aynı", rep("emsc", "e1", 0, 39.21, 28.15, 4.1), 1},
		{"yalnız 30 sn fark", rep("emsc", "e1", 30, 39.21, 28.15, 4.1), 1 - 0.2},
		{"yalnız ΔM=0,5", rep("emsc", "e1", 0, 39.21, 28.15, 4.6), 1 - 0.1},
		{"60 sn sınırda", rep("emsc", "e1", -60, 39.21, 28.15, 4.1), 0.6},
	}
	for _, tt := range tests {
		if got := c.Score(cand, tt.r); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s: puan=%.4f, beklenen %.4f", tt.name, got, tt.want)
		}
	}

	// Mesafe terimi: 0,9° enlem ≈ 100 km -> terim ≈ 0,4.
	far := rep("emsc", "e1", 0, 39.21+0.45, 28.15, 4.1) // ~50 km
	if got := c.Score(cand, far); math.Abs(got-0.8) > 0.01 {
		t.Errorf("~50 km: puan=%.3f, ~0,8 beklenirdi", got)
	}
}

// Plan §1'deki temsilî Sındırgı kayıtları birbirine bağlanmalı.
func TestDecide_Plan1Example(t *testing.T) {
	c := PlanConfig() // formül Plan §8 değerleriyle sınanır
	afad := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	kand := rep("kandilli", "k1", -1, 39.19, 28.17, 4.3)
	emsc := rep("emsc", "e1", 1, 39.23, 28.12, 4.2)

	cand := candOf("E1", afad)
	for _, r := range []normalize.Report{kand, emsc} {
		d := c.Decide(r, []Candidate{cand})
		if d.Action != Attach || d.EventID != "E1" {
			t.Fatalf("%s bağlanmalıydı, karar=%+v", r.Source, d)
		}
		if d.Score < 0.9 {
			t.Errorf("%s puanı beklenenden düşük: %.3f", r.Source, d.Score)
		}
	}
}

func TestDecide(t *testing.T) {
	c := PlanConfig() // formül Plan §8 değerleriyle sınanır
	a := rep("afad", "a1", 0, 39.21, 28.15, 4.1)

	tests := []struct {
		name  string
		r     normalize.Report
		cands []Candidate
		want  Action
	}{
		{"aday yok", a, nil, NewEvent},
		{"pencere dışı zaman (61 sn)", rep("emsc", "e", 61, 39.21, 28.15, 4.1), []Candidate{candOf("E1", a)}, NewEvent},
		{"pencere dışı mesafe (~111 km)", rep("emsc", "e", 0, 40.21, 28.15, 4.1), []Candidate{candOf("E1", a)}, NewEvent},
		{"puan eşiğin altı", rep("emsc", "e", 50, 39.21+0.45, 28.15, 5.0), []Candidate{candOf("E1", a)}, NewEvent},
		{"aynı kaynak, farklı deprem (20 sn, 15 km)", rep("afad", "a2", 20, 39.21+0.135, 28.15, 3.8), []Candidate{candOf("E1", a)}, NewEvent},
		{"aynı kaynak, aynı depremin ikinci id'si", rep("afad", "a2", 1, 39.215, 28.15, 4.1), []Candidate{candOf("E1", a)}, Attach},
		{"aynı rapor (kendisi) üye", a, []Candidate{candOf("E1", a)}, Attach},
	}
	for _, tt := range tests {
		if got := c.Decide(tt.r, tt.cands); got.Action != tt.want {
			t.Errorf("%s: karar=%v (puan %.3f), beklenen %v", tt.name, got.Action, got.Score, tt.want)
		}
	}
}

func TestDecide_PicksBestAndIsOrderIndependent(t *testing.T) {
	c := PlanConfig() // formül Plan §8 değerleriyle sınanır
	r := rep("emsc", "e1", 0, 39.21, 28.15, 4.1)
	near := candOf("B", rep("afad", "a1", 2, 39.21, 28.15, 4.1))
	far := candOf("A", rep("afad", "a2", 30, 39.40, 28.15, 4.0))
	for _, cands := range [][]Candidate{{near, far}, {far, near}} {
		if d := c.Decide(r, cands); d.EventID != "B" {
			t.Fatalf("en yüksek puanlı aday seçilmeliydi: %+v", d)
		}
	}
	// Eşit puanda EventID küçük olan seçilir (aday sırası önemsiz).
	twinA := candOf("A", rep("afad", "a1", 2, 39.21, 28.15, 4.1))
	twinB := candOf("B", rep("kandilli", "k1", 2, 39.21, 28.15, 4.1))
	for _, cands := range [][]Candidate{{twinA, twinB}, {twinB, twinA}} {
		if d := c.Decide(r, cands); d.EventID != "A" {
			t.Fatalf("eşitlikte deterministik seçim bekleniyordu: %+v", d)
		}
	}
}

func TestResolve(t *testing.T) {
	emsc := rep("emsc", "e1", 1, 39.23, 28.12, 4.2)
	emsc.ReceivedAt = t0.Add(10 * time.Second)
	kand := rep("kandilli", "k1", -1, 39.19, 28.17, 4.3)
	kand.ReceivedAt = t0.Add(40 * time.Second)
	afad := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	afad.ReceivedAt = t0.Add(90 * time.Second)
	afad.MagType = "Mw"

	ev := Resolve([]normalize.Report{emsc})
	if ev.Magnitude != 4.2 || ev.Sources[0] != "emsc" {
		t.Fatalf("tek EMSC raporu gösterilmeli: %+v", ev)
	}
	ev = Resolve([]normalize.Report{emsc, kand})
	if ev.Magnitude != 4.3 {
		t.Fatalf("Kandilli EMSC'yi geçmeli: %v", ev.Magnitude)
	}
	ev = Resolve([]normalize.Report{kand, afad, emsc})
	if ev.Magnitude != 4.1 || ev.MagType != "Mw" {
		t.Fatalf("AFAD kazanmalı: %+v", ev)
	}
	if got := ev.Sources; len(got) != 3 || got[0] != "afad" || got[1] != "kandilli" || got[2] != "emsc" {
		t.Fatalf("kaynaklar öncelik sırasında olmalı: %v", got)
	}
	if !ev.FirstReportedAt.Equal(emsc.ReceivedAt) {
		t.Fatalf("FirstReportedAt en erken alınan rapor olmalı: %v", ev.FirstReportedAt)
	}

	// Eşit öncelik: en son alınan kazanır (aynı kaynağın iki id'si).
	afad2 := rep("afad", "a2", 0, 39.22, 28.15, 4.0)
	afad2.ReceivedAt = afad.ReceivedAt.Add(time.Minute)
	if ev := Resolve([]normalize.Report{afad2, afad}); ev.Magnitude != 4.0 {
		t.Fatalf("eşit öncelikte en son alınan kazanmalı: %v", ev.Magnitude)
	}
	if ev := Resolve(nil); !ev.OriginTime.IsZero() {
		t.Fatal("boş girdi sıfır olay dönmeli")
	}
}
