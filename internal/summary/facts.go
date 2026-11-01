// Package summary, olaylar için kısa Türkçe özet üretir ve doğrular (Plan §11).
//
// İlke: model sayı üretmez, sadece cümle kurar.
//
//  1. Facts (deterministik): büyüklük, saat, derinlik, yer, en yakın il ve
//     uzaklık kodla hesaplanır ve özette kullanılabilecek tüm sayılar burada
//     hazır biçimlenmiş olarak bulunur.
//  2. LLM bu nesneden 2–3 cümle yazar ({"ozet": "..."}).
//  3. Validate (deterministik): metindeki her sayı Facts'te birebir var mı,
//     yasak ifade var mı, uzunluk ve şema doğru mu?
//  4. Geçemezse Template: şablonlu özet. Şablon da aynı doğrulayıcıdan geçer.
package summary

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/nisaguven/artci/internal/geo"
	"github.com/nisaguven/artci/internal/normalize"
)

var istanbul = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		return time.FixedZone("TRT", 3*3600)
	}
	return loc
}()

// Facts, özetin dayanabileceği gerçeklerin tamamıdır. Sayısal alanlar
// kullanıcıya gösterileceği biçimde (Türkçe ondalık virgül) string'dir:
// doğrulayıcı metindeki sayıları bu string'lerle birebir karşılaştırır.
type Facts struct {
	Place       string   `json:"yer"`      // kaynak yer adı (sayısız)
	Magnitude   string   `json:"buyukluk"` // "4,1"
	MagType     string   `json:"buyukluk_turu,omitempty"`
	Date        string   `json:"tarih"`                 // "30 Ekim 2026"
	Time        string   `json:"saat"`                  // "15:04" (Türkiye saati)
	DepthKm     string   `json:"derinlik_km,omitempty"` // "7" ya da "7,5"; bilinmiyorsa boş
	NearestCity string   `json:"en_yakin_il"`
	DistanceKm  string   `json:"il_uzakligi_km"` // "35" (5 km'ye yuvarlı)
	Sources     []string `json:"kaynaklar"`      // ["AFAD", "Kandilli"]

	// Ham değerler: şablon ve iç mantık için; LLM'e gönderilmez.
	magValue  float64
	localTime time.Time
}

var sourceNames = map[string]string{
	normalize.SourceAFAD:     "AFAD",
	normalize.SourceKandilli: "Kandilli",
	normalize.SourceEMSC:     "EMSC",
}

var monthsTR = [...]string{"Ocak", "Şubat", "Mart", "Nisan", "Mayıs", "Haziran",
	"Temmuz", "Ağustos", "Eylül", "Ekim", "Kasım", "Aralık"}

// FormatDecimal, tek ondalıklı Türkçe gösterimdir: 4.1 -> "4,1".
func FormatDecimal(v float64) string {
	return strings.Replace(strconv.FormatFloat(math.Round(v*10)/10, 'f', 1, 64), ".", ",", 1)
}

// formatDepth: tam sayıysa ondalıksız ("7"), değilse tek ondalık ("7,5").
func formatDepth(v float64) string {
	r := math.Round(v*10) / 10
	if r == math.Trunc(r) {
		return strconv.Itoa(int(r))
	}
	return FormatDecimal(r)
}

var (
	bracketed = regexp.MustCompile(`\[[^\]]*\]`)
	digits    = regexp.MustCompile(`\d`)
	spaces    = regexp.MustCompile(`\s+`)
)

// cleanPlace, yer adından sayıları ve köşeli parantezli ekleri ayıklar.
// Yer adı kaynaktan gelir (güvenilmez); içinde sayı kalırsa doğrulayıcı her
// özeti reddederdi. Temizlenemezse boş döner ve en yakın il kullanılır.
func cleanPlace(s string) string {
	s = bracketed.ReplaceAllString(s, " ")
	s = strings.Trim(spaces.ReplaceAllString(s, " "), " -,.")
	if s == "" || digits.MatchString(s) || len([]rune(s)) > 80 {
		return ""
	}
	for _, bad := range []string{"<", ">", "&", "{", "}", "\"", "`"} {
		if strings.Contains(s, bad) {
			return ""
		}
	}
	return s
}

// BuildFacts, olaydan gerçekler nesnesini üretir.
func BuildFacts(ev normalize.Event) Facts {
	lt := ev.OriginTime.In(istanbul)
	city, dist := geo.NearestCity(ev.Lat, ev.Lon, 0)
	f := Facts{
		Place:       cleanPlace(ev.PlaceName),
		Magnitude:   FormatDecimal(ev.Magnitude),
		MagType:     ev.MagType,
		Date:        strconv.Itoa(lt.Day()) + " " + monthsTR[lt.Month()-1] + " " + strconv.Itoa(lt.Year()),
		Time:        lt.Format("15:04"),
		NearestCity: city.Name,
		DistanceKm:  strconv.Itoa(roundTo(dist, 5)),
		magValue:    math.Round(ev.Magnitude*10) / 10,
		localTime:   lt,
	}
	if f.Place == "" {
		f.Place = city.Name + " yakınları"
	}
	if ev.DepthKm != nil {
		f.DepthKm = formatDepth(*ev.DepthKm)
	}
	for _, s := range ev.Sources {
		if n, ok := sourceNames[s]; ok {
			f.Sources = append(f.Sources, n)
		}
	}
	return f
}

// roundTo, uzaklığı n'nin katına yuvarlar (en az n). "Yaklaşık" ifadesiyle
// birlikte kullanılır; 37,4 km gibi sahte bir hassasiyet vermeyiz.
func roundTo(v float64, n int) int {
	r := int(math.Round(v/float64(n))) * n
	if r < n {
		return n
	}
	return r
}

// AllowedNumbers, özette geçebilecek sayı belirteçlerinin kümesidir.
// Belirteçler doğrulayıcının normalize ettiği biçimdedir (bkz. normNumber).
func (f Facts) AllowedNumbers() map[string]bool {
	out := map[string]bool{}
	add := func(s string) {
		if s != "" {
			out[normNumber(s)] = true
		}
	}
	add(f.Magnitude)
	add(f.Time)
	add(f.DepthKm)
	add(f.DistanceKm)
	// Tarih: gün ve yıl ayrı sayılar olarak geçer.
	for _, p := range strings.Fields(f.Date) {
		if digits.MatchString(p) {
			add(p)
		}
	}
	return out
}
