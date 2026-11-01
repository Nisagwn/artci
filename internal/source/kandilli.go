package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/nisaguven/artci/internal/normalize"
)

// KandilliURL: KOERI "son depremler" sayfası (HTML içinde <pre> metin tablosu).
const KandilliURL = "http://www.koeri.boun.edu.tr/scripts/lst0.asp"

// Türkiye 2016'dan beri sabit UTC+3 kullanır; Kandilli saatleri yereldir.
var turkeyTZ = time.FixedZone("TRT", 3*3600)

// Tablo satırı:
//
//	2026.10.30 15:04:30  39.1900   28.1700        7.0      -.-  4.3  -.-   SINDIRGI (BALIKESIR)      İlksel
//
// Yer adı tek boşluk içerebilir; çözüm niteliğinden en az iki boşlukla ayrılır.
var (
	kandilliDateStart = regexp.MustCompile(`^\s*\d{4}\.\d{2}\.\d{2}\s`)
	kandilliRow       = regexp.MustCompile(
		`^\s*(\d{4}\.\d{2}\.\d{2})\s+(\d{2}:\d{2}:\d{2})\s+(-?\d+(?:\.\d+)?)\s+(-?\d+(?:\.\d+)?)\s+(-?\d+(?:\.\d+)?)\s+` +
			`(\S+)\s+(\S+)\s+(\S+)\s+(.+?)(?:\s{2,}(\S.*?))?\s*$`)
)

// kandilliRaw, DLQ ve reports.raw için satırın yapılandırılmış hâlidir.
type kandilliRaw struct {
	Line     string `json:"line"`
	Solution string `json:"solution,omitempty"`
	MD       string `json:"md"`
	ML       string `json:"ml"`
	Mw       string `json:"mw"`
}

// decodeKandilli, sayfa UTF-8 değilse Windows-1254 kabul edip çevirir.
func decodeKandilli(body []byte) string {
	if utf8.Valid(body) {
		return string(body)
	}
	out, err := charmap.Windows1254.NewDecoder().Bytes(body)
	if err != nil {
		return string(bytes.ToValidUTF8(body, []byte("?")))
	}
	return string(out)
}

// preBlock, <pre>...</pre> arasını döner; etiket yoksa tüm metni.
func preBlock(s string) string {
	// strings.ToLower kullanılmaz: "İ" küçülünce 2 bayttan 3 bayta çıkar ve
	// lower'daki konumlar s'ye uymaz (gerçek sayfada tablonun sonu kesiliyordu).
	lower := asciiLower(s)
	i := strings.Index(lower, "<pre")
	if i < 0 {
		return s
	}
	if j := strings.Index(lower[i:], ">"); j >= 0 {
		i += j + 1
	}
	if k := strings.Index(lower[i:], "</pre>"); k >= 0 {
		return s[i : i+k]
	}
	return s[i:]
}

// asciiLower yalnızca A-Z'yi küçültür; bayt uzunluğu değişmez.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// ParseKandilli, sayfayı raporlara çevirir. Tarihle başlamayan satırlar
// (başlık, ayraç) yok sayılır; tarihle başlayıp çözülemeyen satırlar BadRecord olur.
// Tablo hiç bulunamazsa hata döner: sayfa biçimi değişmiş demektir.
func ParseKandilli(body []byte, receivedAt time.Time) (Result, error) {
	text := preBlock(decodeKandilli(body))
	var reports []normalize.Report
	var bad []BadRecord
	seen := map[string]int{}
	rows := 0
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if !kandilliDateStart.MatchString(line) {
			continue
		}
		rows++
		r, err := parseKandilliLine(line, receivedAt)
		if err != nil {
			bad = append(bad, BadRecord{Raw: line, Reason: err.Error()})
			continue
		}
		seen[r.SourceEventID]++
		reports = append(reports, r)
	}
	if rows == 0 {
		return Result{}, fmt.Errorf("kandilli: tabloda hiç satır yok (sayfa biçimi değişmiş olabilir)")
	}
	// Aynı saniyede iki deprem: kimliği konumla ayrıştır (nadir).
	for i := range reports {
		if seen[reports[i].SourceEventID] > 1 {
			reports[i].SourceEventID += fmt.Sprintf("_%.4f_%.4f", reports[i].Lat, reports[i].Lon)
		}
	}
	return finish(reports, bad), nil
}

func parseKandilliLine(line string, receivedAt time.Time) (normalize.Report, error) {
	m := kandilliRow.FindStringSubmatch(line)
	if m == nil {
		return normalize.Report{}, fmt.Errorf("kandilli: satır biçimi tanınmadı")
	}
	t, err := time.ParseInLocation("2006.01.02 15:04:05", m[1]+" "+m[2], turkeyTZ)
	if err != nil {
		return normalize.Report{}, fmt.Errorf("kandilli: tarih: %w", err)
	}
	lat, err1 := strconv.ParseFloat(m[3], 64)
	lon, err2 := strconv.ParseFloat(m[4], 64)
	depth, err3 := strconv.ParseFloat(m[5], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return normalize.Report{}, fmt.Errorf("kandilli: sayısal alan çözülemedi")
	}
	mag, magType, ok := pickKandilliMag(m[6], m[7], m[8])
	if !ok {
		return normalize.Report{}, fmt.Errorf("kandilli: büyüklük yok (MD=%s ML=%s Mw=%s)", m[6], m[7], m[8])
	}
	raw, _ := json.Marshal(kandilliRaw{
		Line: strings.TrimRight(line, " \t"), Solution: strings.TrimSpace(m[10]),
		MD: m[6], ML: m[7], Mw: m[8],
	})
	utc := t.UTC()
	return normalize.Report{
		Source: normalize.SourceKandilli,
		// Kandilli kimlik vermez. Revizyonlarda konum/büyüklük değişebilir ama
		// oluş zamanı sabit kalır; bu yüzden kimlik zamandan türetilir ve
		// revizyon aynı raporun güncellemesi olarak upsert edilir.
		SourceEventID: utc.Format("20060102150405"),
		OriginTime:    utc,
		Lat:           lat,
		Lon:           lon,
		DepthKm:       &depth,
		Magnitude:     mag,
		MagType:       magType,
		PlaceName:     strings.TrimSpace(m[9]),
		Raw:           raw,
		ReceivedAt:    receivedAt.UTC(),
	}, nil
}

// pickKandilliMag: tercih sırası Mw > ML > MD. "-.-" değer yok demektir.
func pickKandilliMag(md, ml, mw string) (float64, string, bool) {
	for _, c := range []struct{ v, typ string }{{mw, "Mw"}, {ml, "ML"}, {md, "MD"}} {
		if v, err := strconv.ParseFloat(c.v, 64); err == nil && v > 0 {
			return v, c.typ, true
		}
	}
	return 0, "", false
}

// Kandilli, KOERI sayfasını sorgulayan Fetcher'dır.
type Kandilli struct {
	HTTP *HTTPClient
	URL  string // boşsa KandilliURL
	Now  func() time.Time
}

func (k *Kandilli) Name() string { return normalize.SourceKandilli }

func (k *Kandilli) Fetch(ctx context.Context) (Result, error) {
	u := k.URL
	if u == "" {
		u = KandilliURL
	}
	now := time.Now
	if k.Now != nil {
		now = k.Now
	}
	body, err := k.HTTP.Get(ctx, u)
	if err != nil {
		return Result{}, err
	}
	return ParseKandilli(body, now())
}
