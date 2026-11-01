package source

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

var recv = time.Date(2026, 10, 30, 12, 6, 0, 0, time.UTC)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/source/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseAFAD(t *testing.T) {
	res, err := ParseAFAD(readFixture(t, "afad.json"), recv)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 3 || len(res.Bad) != 3 {
		t.Fatalf("rapor=%d bozuk=%d, beklenen 3/3; bozuklar: %+v", len(res.Reports), len(res.Bad), res.Bad)
	}
	r := res.Reports[0]
	want := time.Date(2026, 10, 30, 12, 4, 31, 0, time.UTC)
	if r.Source != "afad" || r.SourceEventID != "702311" || !r.OriginTime.Equal(want) ||
		r.Lat != 39.21 || r.Lon != 28.15 || r.Magnitude != 4.1 || r.MagType != "ML" ||
		r.DepthKm == nil || *r.DepthKm != 7.02 || r.PlaceName != "Sındırgı (Balıkesir)" || !r.ReceivedAt.Equal(recv) {
		t.Errorf("ilk rapor yanlış: %+v", r)
	}
	if r.OriginTime.Location() != time.UTC {
		t.Errorf("oluş zamanı UTC olmalı")
	}
	if got := res.Reports[2].PlaceName; got != "Yeşilyurt (Malatya)" {
		t.Errorf("location boşken il/ilçe kullanılmalı, %q", got)
	}
	reasons := ""
	for _, b := range res.Bad {
		reasons += b.Reason + "\n"
	}
	for _, w := range []string{"eksik", "tarih", "büyüklük"} {
		if !strings.Contains(reasons, w) {
			t.Errorf("%q içeren bir DLQ nedeni bekleniyordu:\n%s", w, reasons)
		}
	}
}

func TestParseAFAD_NotArray(t *testing.T) {
	if _, err := ParseAFAD([]byte(`{"error":"rate limited"}`), recv); err == nil {
		t.Error("dizi olmayan yanıt kaynak hatası sayılmalı")
	}
}

func TestParseAFAD_NumericFields(t *testing.T) {
	body := `[{"eventID":7,"date":"2026-10-30T12:04:31","latitude":39.2,"longitude":28.1,"depth":null,"magnitude":3,"type":"ML"}]`
	res, err := ParseAFAD([]byte(body), recv)
	if err != nil || len(res.Reports) != 1 {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if r := res.Reports[0]; r.SourceEventID != "7" || r.DepthKm != nil || r.Magnitude != 3 {
		t.Errorf("sayısal alanlar yanlış: %+v", r)
	}
}

func TestParseKandilli(t *testing.T) {
	res, err := ParseKandilli(readFixture(t, "kandilli.html"), recv)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 3 || len(res.Bad) != 2 {
		t.Fatalf("rapor=%d bozuk=%d, beklenen 3/2; bozuklar: %+v", len(res.Reports), len(res.Bad), res.Bad)
	}
	r := res.Reports[0]
	// 15:04:30 Türkiye saati = 12:04:30 UTC.
	want := time.Date(2026, 10, 30, 12, 4, 30, 0, time.UTC)
	if !r.OriginTime.Equal(want) || r.SourceEventID != "20261030120430" ||
		r.Lat != 39.19 || r.Lon != 28.17 || r.Magnitude != 4.3 || r.MagType != "ML" ||
		*r.DepthKm != 7 || r.PlaceName != "SINDIRGI (BALIKESIR)" {
		t.Errorf("ilk rapor yanlış: %+v", r)
	}
	if !strings.Contains(string(r.Raw), "İlksel") {
		t.Errorf("raw çözüm niteliğini içermeli: %s", r.Raw)
	}
	// Mw varsa Mw tercih edilir; revize satırının yer adı tek parça kalmalı.
	if e := res.Reports[1]; e.Magnitude != 3.3 || e.MagType != "Mw" || e.PlaceName != "EGE DENIZI" ||
		!strings.Contains(string(e.Raw), "REVIZE01") {
		t.Errorf("revize satırı yanlış: %+v raw=%s", e, e.Raw)
	}
	if m := res.Reports[2]; m.Magnitude != 2.2 || m.MagType != "MD" {
		t.Errorf("yalnız MD varken MD kullanılmalı: %+v", m)
	}
}

func TestParseKandilli_Windows1254(t *testing.T) {
	enc, err := charmap.Windows1254.NewEncoder().Bytes(readFixture(t, "kandilli.html"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := ParseKandilli(enc, recv)
	if err != nil || len(res.Reports) != 3 {
		t.Fatalf("err=%v rapor=%d", err, len(res.Reports))
	}
	if !strings.Contains(string(res.Reports[0].Raw), "İlksel") {
		t.Errorf("Windows-1254 çözülmemiş: %s", res.Reports[0].Raw)
	}
}

func TestParseKandilli_NoTable(t *testing.T) {
	if _, err := ParseKandilli([]byte("<html>Bakım çalışması</html>"), recv); err == nil {
		t.Error("tablosuz sayfa kaynak hatası sayılmalı")
	}
}

func TestParseKandilli_SameSecond(t *testing.T) {
	page := "<pre>\n" +
		"2026.10.30 15:04:30  39.1900   28.1700        7.0      -.-  4.3  -.-   SINDIRGI (BALIKESIR)      İlksel\n" +
		"2026.10.30 15:04:30  36.0000   36.1000        9.0      -.-  2.1  -.-   ANTAKYA (HATAY)           İlksel\n" +
		"</pre>"
	res, err := ParseKandilli([]byte(page), recv)
	if err != nil || len(res.Reports) != 2 {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if res.Reports[0].SourceEventID == res.Reports[1].SourceEventID {
		t.Error("aynı saniyedeki iki depremin kimliği farklı olmalı")
	}
}

func TestParseEMSCFDSN(t *testing.T) {
	res, err := ParseEMSCFDSN(readFixture(t, "emsc_fdsn.json"), recv)
	if err != nil {
		t.Fatal(err)
	}
	// Japonya kaydı bölge dışı: ne rapor ne DLQ.
	if len(res.Reports) != 1 || len(res.Bad) != 1 {
		t.Fatalf("rapor=%d bozuk=%d, beklenen 1/1", len(res.Reports), len(res.Bad))
	}
	r := res.Reports[0]
	want := time.Date(2026, 10, 30, 12, 4, 32, 100e6, time.UTC)
	if r.Source != "emsc" || r.SourceEventID != "20261030_0000120" || !r.OriginTime.Equal(want) ||
		r.Magnitude != 4.2 || r.MagType != "ml" || r.PlaceName != "WESTERN TURKEY" {
		t.Errorf("rapor yanlış: %+v", r)
	}
}

func TestParseEMSCMessage(t *testing.T) {
	sc := bufio.NewScanner(bytes.NewReader(readFixture(t, "emsc_ws.jsonl")))
	var mags []float64
	for sc.Scan() {
		r, err := ParseEMSCMessage(sc.Bytes(), recv)
		if err != nil {
			t.Fatal(err)
		}
		mags = append(mags, r.Magnitude)
	}
	if len(mags) != 2 || mags[0] != 3.4 || mags[1] != 3.3 {
		t.Errorf("büyüklükler %v", mags)
	}
	if _, err := ParseEMSCMessage([]byte(`{"action":"create"}`), recv); err == nil {
		t.Error("data'sız mesaj hata olmalı")
	}
	if _, err := ParseEMSCMessage([]byte(`{not json`), recv); err == nil {
		t.Error("bozuk JSON hata olmalı")
	}
}
