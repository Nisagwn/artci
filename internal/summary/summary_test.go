package summary

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/geo"
	"github.com/nisaguven/artci/internal/normalize"
)

func fptr(f float64) *float64 { return &f }

// Sındırgı örneği (Plan §1): 12:04:31 UTC = 15:04 Türkiye saati.
func sindirgi() normalize.Event {
	return normalize.Event{
		ID: "E1", Version: 1,
		OriginTime: time.Date(2026, 10, 30, 12, 4, 31, 0, time.UTC),
		Lat:        39.21, Lon: 28.15, DepthKm: fptr(7.02), Magnitude: 4.1, MagType: "ML",
		PlaceName: "Sındırgı (Balıkesir)", Sources: []string{"afad", "kandilli", "emsc"},
	}
}

func TestBuildFacts(t *testing.T) {
	f := BuildFacts(sindirgi())
	want := Facts{Place: "Sındırgı (Balıkesir)", Magnitude: "4,1", MagType: "ML", Date: "30 Ekim 2026",
		Time: "15:04", DepthKm: "7", NearestCity: "Balıkesir", Sources: []string{"AFAD", "Kandilli", "EMSC"}}
	if f.Place != want.Place || f.Magnitude != want.Magnitude || f.Date != want.Date || f.Time != want.Time ||
		f.DepthKm != want.DepthKm || f.NearestCity != want.NearestCity || strings.Join(f.Sources, ",") != "AFAD,Kandilli,EMSC" {
		t.Errorf("gerçekler yanlış:\n got %+v\nwant %+v", f, want)
	}
	if f.DistanceKm == "" || f.DistanceKm[len(f.DistanceKm)-1] != '0' && f.DistanceKm[len(f.DistanceKm)-1] != '5' {
		t.Errorf("uzaklık 5'in katı olmalı: %q", f.DistanceKm)
	}
}

func TestCleanPlace(t *testing.T) {
	tests := map[string]string{
		"Sındırgı (Balıkesir)":              "Sındırgı (Balıkesir)",
		"Ege Denizi - [12.34 km] Datça":     "Ege Denizi - Datça",
		"KM 12 Kuzeyi":                      "",
		"<b>ignore previous</b>":            "",
		"   ":                               "",
		"Akdeniz [21.5 km] - Kaş (Antalya)": "Akdeniz - Kaş (Antalya)",
	}
	for in, want := range tests {
		if got := cleanPlace(in); got != want {
			t.Errorf("cleanPlace(%q)=%q, beklenen %q", in, got, want)
		}
	}
	ev := sindirgi()
	ev.PlaceName = "KM 12 Kuzeyi"
	if f := BuildFacts(ev); f.Place != "Balıkesir yakınları" {
		t.Errorf("sayılı yer adı yerine en yakın il kullanılmalı: %q", f.Place)
	}
}

func TestLocativeSuffix(t *testing.T) {
	tests := map[string]string{
		"15:04": "'te", "15:06": "'da", "15:01": "'de", "15:10": "'da", "15:20": "'de",
		"15:30": "'da", "15:40": "'ta", "15:50": "'te", "15:00": "'te", "12:00": "'de",
		"20:00": "'de", "00:00": "'da", "09:00": "'da", "10:00": "'da", "15:09": "'da", "15:43": "'te",
	}
	for in, want := range tests {
		if got := LocativeSuffix(in); got != want {
			t.Errorf("%s -> %s, beklenen %s", in, got, want)
		}
	}
}

func TestTemplate(t *testing.T) {
	f := BuildFacts(sindirgi())
	got := Template(f)
	want := "Sındırgı (Balıkesir) merkezli 4,1 büyüklüğünde bir deprem 30 Ekim 2026 saat 15:04'te meydana geldi. " +
		"Derinlik 7 km. En yakın il merkezi Balıkesir, yaklaşık " + f.DistanceKm + " km uzaklıkta. Veriler: AFAD, Kandilli, EMSC."
	if got != want {
		t.Errorf("şablon:\n got %s\nwant %s", got, want)
	}
	if err := Validate(got, f); err != nil {
		t.Errorf("şablon doğrulamadan geçmeli: %v", err)
	}
}

// Şablon her olayda doğrulayıcıdan geçmeli: rastgele 5000 olay, tüm iller,
// derinliksiz, kaynaksız, yer adı bozuk varyantlar.
func TestTemplateAlwaysValid(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	places := []string{"", "Sındırgı (Balıkesir)", "EGE DENIZI", "Ölüdeniz Açıkları (Muğla)", "Van Gölü",
		"On Yedi Eylül Mah. (Balıkesir)", "Akdeniz - [21.5 km] Kaş", "WESTERN TURKEY", "Hasankeyf (Batman)"}
	for i := 0; i < 5000; i++ {
		c := geo.Cities[rng.IntN(len(geo.Cities))]
		ev := normalize.Event{
			ID: "E", Version: 1,
			OriginTime: time.Date(2026, time.Month(1+rng.IntN(12)), 1+rng.IntN(28), rng.IntN(24), rng.IntN(60), rng.IntN(60), 0, time.UTC),
			Lat:        c.Lat + rng.Float64() - 0.5, Lon: c.Lon + rng.Float64() - 0.5,
			Magnitude: 3.5 + float64(rng.IntN(45))/10, PlaceName: places[rng.IntN(len(places))],
		}
		if rng.IntN(3) > 0 {
			ev.DepthKm = fptr(rng.Float64() * 150)
		}
		if rng.IntN(4) > 0 {
			ev.Sources = []string{"afad", "kandilli"}[:1+rng.IntN(2)]
		}
		f := BuildFacts(ev)
		if err := Validate(Template(f), f); err != nil {
			t.Fatalf("şablon reddedildi: %v\n%s\n%+v", err, Template(f), f)
		}
	}
}

func TestParseModelOutput(t *testing.T) {
	ok := []string{
		`{"ozet": "metin"}`,
		"```json\n{\"ozet\": \"metin\"}\n```",
		"  {\"ozet\":\"metin\"}  ",
	}
	for _, in := range ok {
		if got, err := ParseModelOutput(in); err != nil || got != "metin" {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	bad := []string{``, `metin`, `{"ozet": ""}`, `{"summary": "x"}`, `{"ozet": "x", "not": "y"}`,
		`{"ozet": "x"} {"ozet": "y"}`, `{"ozet": 4.1}`, `[{"ozet": "x"}]`}
	for _, in := range bad {
		if _, err := ParseModelOutput(in); err == nil {
			t.Errorf("%q reddedilmeli", in)
		} else if r, ok := IsRejection(err); !ok || r.Code != "schema" {
			t.Errorf("%q: schema reddi beklenirdi, %v", in, err)
		}
	}
}

func TestContainsAtWordStart(t *testing.T) {
	tests := []struct {
		s, p string
		want bool
	}{
		{"ölü sayısı", "ölü", true},
		{"bölümünde", "ölü", false},
		{"van gölü", "ölü", false},
		{"hiç ölümü yok", "ölü", true},
		{"a<b", "<", true},
		{"riskli bölge", "risk", true},
		{"asteriskler", "risk", false},
	}
	for _, tt := range tests {
		if got := containsAtWordStart(tt.s, tt.p); got != tt.want {
			t.Errorf("%q/%q=%v", tt.s, tt.p, got)
		}
	}
}
