package replay

import (
	"bytes"
	"math"
	"testing"
	"time"
)

func TestGenerateDeterministicAndPlausible(t *testing.T) {
	c := DefaultGenConfig()
	c.Count = 300
	a, b := Generate(c), Generate(c)
	if len(a) != len(b) || a[10].Report.SourceEventID != b[10].Report.SourceEventID {
		t.Fatal("aynı tohum aynı kataloğu vermeli")
	}
	truths := map[string]bool{}
	ids := map[string]int{}
	early := 0
	for i, e := range a {
		if err := e.Report.Validate(); err != nil {
			t.Fatalf("geçersiz rapor: %v %+v", err, e.Report)
		}
		if i > 0 && e.ArriveS < a[i-1].ArriveS {
			t.Fatal("varış sırasına dizili olmalı")
		}
		truths[e.Truth] = true
		ids[e.Report.Source+"/"+e.Report.SourceEventID]++
		if e.ArriveS < 3600 {
			early++
		}
	}
	// Eşik altı ve erken dönem eksikliği yüzünden hepsi görünmez; yarısından fazlası görünmeli.
	if len(truths) < c.Count/2 {
		t.Errorf("depremlerin çoğu en az bir kaynakta görünmeli: %d/%d", len(truths), c.Count)
	}
	revisions := 0
	for _, n := range ids {
		if n > 1 {
			revisions++
		}
	}
	if revisions == 0 {
		t.Error("revizyon raporları olmalı")
	}
	// Omori: 24 saatlik dizide düzgün dağılım ilk saate ~%4 verirdi.
	if frac := float64(early) / float64(len(a)); frac < 0.2 {
		t.Errorf("ilk saatteki rapor oranı %.2f; Omori bozunumu beklenirdi", frac)
	}
}

func TestCatalogRoundTrip(t *testing.T) {
	c := DefaultGenConfig()
	c.Count = 20
	in := Generate(c)
	var buf bytes.Buffer
	if err := WriteCatalog(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadCatalog(&buf)
	if err != nil || len(out) != len(in) || !out[3].Report.OriginTime.Equal(in[3].Report.OriginTime) {
		t.Fatalf("gidiş-dönüş bozuldu: %v", err)
	}
	if _, err := ReadCatalog(bytes.NewBufferString(`{"arrive_s":1}`)); err == nil {
		t.Error("truth'suz satır reddedilmeli")
	}
}

func TestScore(t *testing.T) {
	a := func(key, truth, ev, src string) Assignment { return Assignment{key, truth, ev, src} }
	perfect := []Assignment{
		a("1", "T1", "E1", "afad"), a("2", "T1", "E1", "kandilli"), a("3", "T1", "E1", "emsc"),
		a("4", "T2", "E2", "afad"), a("5", "T2", "E2", "emsc"),
	}
	if s := Score(perfect); s.Precision != 1 || s.Recall != 1 || s.ExactlyCorrect != 1 || s.MergedEvents != 2 {
		t.Errorf("mükemmel: %+v", s)
	}
	// T1 ikiye bölünmüş: recall düşer, precision 1 kalır.
	split := []Assignment{
		a("1", "T1", "E1", "afad"), a("2", "T1", "E1", "kandilli"), a("3", "T1", "E9", "emsc"),
		a("4", "T2", "E2", "afad"), a("5", "T2", "E2", "emsc"),
	}
	s := Score(split)
	if s.Precision != 1 || math.Abs(s.Recall-2.0/4) > 1e-9 || s.ExactlyCorrect != 0.5 {
		t.Errorf("bölünmüş: %+v", s)
	}
	// T1 ve T2 yanlışlıkla birleşmiş: precision düşer.
	merged := []Assignment{
		a("1", "T1", "E1", "afad"), a("2", "T1", "E1", "kandilli"),
		a("4", "T2", "E1", "emsc"),
	}
	s = Score(merged)
	if s.Recall != 1 || math.Abs(s.Precision-1.0/3) > 1e-9 || s.ExactlyCorrect != 0 {
		t.Errorf("birleşmiş: %+v", s)
	}
}

func TestSummarize(t *testing.T) {
	var s []time.Duration
	for i := 1; i <= 100; i++ {
		s = append(s, time.Duration(i)*time.Millisecond)
	}
	p := Summarize(s)
	if p.P50 != 50*time.Millisecond || p.P95 != 95*time.Millisecond || p.P99 != 99*time.Millisecond || p.Max != 100*time.Millisecond {
		t.Errorf("%+v", p)
	}
	if (Summarize(nil) != Percentiles{}) {
		t.Error("boş")
	}
}
