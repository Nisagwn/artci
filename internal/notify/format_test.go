package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/geo"
	"github.com/nisaguven/artci/internal/normalize"
)

func ptr(v float64) *float64 { return &v }

// sindirgi, Plan §4 Senaryo 1'deki temsilî olaydır.
func sindirgi(mag float64) normalize.Event {
	return normalize.Event{
		ID:         "11111111-1111-1111-1111-111111111111",
		OriginTime: time.Date(2026, 10, 31, 9, 4, 31, 0, time.UTC), // 12:04 TSİ
		Lat:        39.24, Lon: 28.17,
		DepthKm:   ptr(10.2),
		Magnitude: mag, MagType: "Mw",
		PlaceName: "Sındırgı (Balıkesir)",
		Sources:   []string{"emsc", "kandilli", "afad"},
	}
}

func balikesir() Target {
	c, _ := geo.FindCity("Balıkesir")
	return Target{City: c.Name, Lat: c.Lat, Lon: c.Lon}
}

func TestFormatEventScenario1(t *testing.T) {
	got := FormatEvent(sindirgi(4.1), balikesir(), nil)
	want := "🟠 <b>Sındırgı (Balıkesir), Mw 4,1</b> · 12:04 · derinlik 10,2 km\n" +
		"Balıkesir merkezine yaklaşık 52 km uzaklıkta. Bölgede hafif hissedilmiş olabilir.\n" +
		"Kaynaklar: AFAD, Kandilli, EMSC · Resmî bilgi: AFAD"
	if got != want {
		t.Fatalf("mesaj farklı:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatEventVariants(t *testing.T) {
	t.Run("güncelleme önceki değeri yazar", func(t *testing.T) {
		got := FormatEvent(sindirgi(4.6), balikesir(), ptr(4.1))
		for _, s := range []string{"🟠 <b>Güncelleme · Sındırgı (Balıkesir), Mw 4,6</b>", "önceki bildirim: 4,1"} {
			if !strings.Contains(got, s) {
				t.Errorf("%q içermiyor:\n%s", s, got)
			}
		}
	})
	t.Run("yer adı kaçışlı, derinlik yok, tür yok", func(t *testing.T) {
		ev := sindirgi(3.6)
		ev.PlaceName, ev.DepthKm, ev.MagType, ev.Sources = "A<b>&B", nil, "", nil
		got := FormatEvent(ev, balikesir(), nil)
		if !strings.HasPrefix(got, "🟡 <b>A&lt;b&gt;&amp;B, M 3,6</b> · 12:04\n") {
			t.Errorf("ilk satır beklenmedik: %q", got)
		}
		if strings.Contains(got, "derinlik") || strings.Contains(got, "Kaynaklar:") {
			t.Errorf("olmayan alanlar yazılmamalı: %q", got)
		}
		if !strings.HasSuffix(got, "Resmî bilgi: AFAD") {
			t.Errorf("resmî bilgi satırı yok: %q", got)
		}
	})
	t.Run("yer adı yoksa en yakın il", func(t *testing.T) {
		ev := sindirgi(4.0)
		ev.PlaceName = ""
		if got := FormatEvent(ev, balikesir(), nil); !strings.Contains(got, "Balıkesir yakınları, Mw 4,0") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("panik ve tahmin dili yok", func(t *testing.T) {
		for _, m := range []float64{3.5, 4.5, 5.5, 7.2} {
			got := strings.ToLower(FormatEvent(sindirgi(m), balikesir(), nil))
			for _, bad := range []string{"felaket", "yıkım", "gelecek", "beklenen", "tehlike", "acil", "!"} {
				if strings.Contains(got, bad) {
					t.Errorf("M%.1f mesajında %q var: %s", m, bad, got)
				}
			}
		}
	})
}

func TestMagnitudeEmoji(t *testing.T) {
	tests := []struct {
		mag  float64
		want string
	}{
		{2.5, "🟡"}, {3.99, "🟠"}, {3.9, "🟡"}, {4.0, "🟠"}, {4.9, "🟠"}, {5.0, "🔴"}, {7.8, "🔴"},
	}
	for _, tt := range tests {
		if got := MagnitudeEmoji(tt.mag); got != tt.want {
			t.Errorf("MagnitudeEmoji(%v) = %s, want %s", tt.mag, got, tt.want)
		}
	}
}

func TestFeltPhrase(t *testing.T) {
	const wide, light, none = "Bölgede geniş bir alanda hissedilmiş olabilir.", "Bölgede hafif hissedilmiş olabilir.", "Bu uzaklıkta büyük olasılıkla hissedilmemiştir."
	tests := []struct {
		mag, km float64
		want    string
	}{
		{4.1, 62, light}, // Plan §4 Senaryo 1
		{4.1, 10, wide},
		{4.1, 150, none},
		{3.5, 30, light},
		{3.5, 60, none},
		{5.2, 80, wide},
		{5.2, 200, light},
		{6.5, 400, light},
		{6.5, 600, none},
	}
	for _, tt := range tests {
		if got := FeltPhrase(tt.mag, tt.km); got != tt.want {
			t.Errorf("FeltPhrase(%v, %v) = %q, want %q", tt.mag, tt.km, got, tt.want)
		}
	}
}

func TestIstanbulTime(t *testing.T) {
	// Kış saati yok: Türkiye 2016'dan beri sürekli UTC+3.
	if got := time.Date(2026, 1, 15, 21, 30, 0, 0, time.UTC).In(istanbul).Format("15:04"); got != "00:30" {
		t.Fatalf("got %s", got)
	}
}
