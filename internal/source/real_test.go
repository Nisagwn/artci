package source

import (
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/source/real/ altındaki dosyalar 31.10.2026'da kaynaklardan çekilmiş
// gerçek yanıtlardır (küçültülmüş). Aynı depremin üç kaynakta aynı UTC anına
// düşmesi, saat dilimi varsayımlarını (AFAD UTC, Kandilli lst0 TRT) doğrular.

func readReal(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/source/real/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func byID(t *testing.T, r Result, id string) time.Time {
	t.Helper()
	for _, x := range r.Reports {
		if x.SourceEventID == id {
			return x.OriginTime
		}
	}
	t.Fatalf("%s bulunamadı", id)
	return time.Time{}
}

func TestReal_AllRowsParse(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name string
		want int
		f    func([]byte, time.Time) (Result, error)
	}{
		{"afad_20261031.json", 30, ParseAFAD},
		{"kandilli_lst0_20261031.html", 30, ParseKandilli},
		{"emsc_fdsn_20261031.json", 30, ParseEMSCFDSN},
	} {
		r, err := c.f(readReal(t, c.name), now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(r.Reports) != c.want || len(r.Bad) != 0 {
			t.Errorf("%s: %d rapor, %d bozuk; beklenen %d, 0", c.name, len(r.Reports), len(r.Bad), c.want)
		}
	}
	if _, err := ParseEMSCMessage(readReal(t, "emsc_ws_20261031.json"), now); err != nil {
		t.Errorf("emsc ws: %v", err)
	}
}

func TestReal_SameQuakeSameUTC(t *testing.T) {
	now := time.Now()
	afad, _ := ParseAFAD(readReal(t, "afad_20261031.json"), now)
	kand, _ := ParseKandilli(readReal(t, "kandilli_lst0_20261031.html"), now)
	emsc, _ := ParseEMSCFDSN(readReal(t, "emsc_fdsn_20261031.json"), now)

	// EMSC kaydının yazarı AFAD: saniyesi saniyesine aynı olmalı.
	a := byID(t, afad, "597938")
	if e := byID(t, emsc, "20261031_0000259"); !a.Equal(e) {
		t.Errorf("AFAD %v, EMSC %v", a, e)
	}
	// Kandilli sayfada 21:29:34 (TRT) yazar; AFAD 18:29:34 UTC.
	want := time.Date(2026, 10, 31, 18, 29, 34, 0, time.UTC)
	if k := byID(t, kand, "20261031182934"); !k.Equal(want) {
		t.Errorf("Kandilli %v, beklenen %v", k, want)
	}
}

// Gerçek sayfa 500 satır ve her satırda "İlksel" içerir. strings.ToLower ile
// aranan </pre> konumu kayıyor, tablonun son satırları sessizce düşüyordu.
func TestReal_KandilliFullPageKeepsLastRows(t *testing.T) {
	s := string(readReal(t, "kandilli_lst0_20261031.html"))
	i := strings.Index(s, "2026.10.30 22:57:09")
	row := s[i : i+strings.Index(s[i:], "\n")+1]
	big := s[:i] + strings.Repeat(row, 470) + s[i:]

	r, err := ParseKandilli([]byte(big), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Reports) != 500 || len(r.Bad) != 0 {
		t.Fatalf("%d rapor, %d bozuk; beklenen 500, 0", len(r.Reports), len(r.Bad))
	}
	if last := r.Reports[len(r.Reports)-1]; last.PlaceName != "BULGARISTAN" {
		t.Errorf("son satır %q, beklenen BULGARISTAN", last.PlaceName)
	}
}
