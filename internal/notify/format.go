package notify

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // Windows'ta ve minimal imajlarda Europe/Istanbul bulunsun diye

	"github.com/nisaguven/artci/internal/geo"
	"github.com/nisaguven/artci/internal/normalize"
)

// istanbul: kullanıcıya gösterilen saat her zaman Türkiye saatidir.
var istanbul = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		// tzdata gömülü olduğu için olmamalı; yine de UTC+3'e düş.
		return time.FixedZone("TRT", 3*60*60)
	}
	return loc
}()

// Disclaimer: /start ve yardım metninde birebir geçer (Plan §17).
const Disclaimer = "Bu bir eğitim projesidir; resmî bilgi için AFAD'ı takip edin."

// shortDisclaimer: her komut yanıtının sonuna eklenen kısa uyarı.
const shortDisclaimer = "<i>Eğitim projesidir · Resmî bilgi: AFAD</i>"

// MagnitudeEmoji, büyüklük seviyesini gösterir; emoji yalnızca seviye içindir (Plan §17).
func MagnitudeEmoji(mag float64) string {
	switch m := round1(mag); {
	case m >= 5:
		return "🔴"
	case m >= 4:
		return "🟠"
	default:
		return "🟡"
	}
}

// feltBand: büyüklüğe göre kabaca "geniş alanda" ve "hafif" hissedilme yarıçapları (km).
// Bilimsel bir şiddet modeli değil; yalnızca sakin, temkinli bir ifade seçmek için.
type feltBand struct {
	minMag          float64
	wideKm, lightKm float64
}

var feltBands = []feltBand{ // büyükten küçüğe
	{6, 250, 500},
	{5, 100, 250},
	{4, 20, 100},
	{0, 0, 40},
}

// FeltPhrase, uzaklık ve büyüklüğe göre sakin bir "hissedilmiş olabilir" cümlesi döner.
// Panik dili ve tahmin içermez; her zaman "olabilir" kipindedir.
func FeltPhrase(mag, distKm float64) string {
	m := round1(mag)
	for _, b := range feltBands {
		if m < b.minMag {
			continue
		}
		switch {
		case distKm <= b.wideKm:
			return "Bölgede geniş bir alanda hissedilmiş olabilir."
		case distKm <= b.lightKm:
			return "Bölgede hafif hissedilmiş olabilir."
		default:
			return "Bu uzaklıkta büyük olasılıkla hissedilmemiştir."
		}
	}
	return ""
}

// sourceNames: kaynak kimliği -> gösterilen ad.
var sourceNames = map[string]string{
	normalize.SourceAFAD:     "AFAD",
	normalize.SourceKandilli: "Kandilli",
	normalize.SourceEMSC:     "EMSC",
}

// sourcesLine, kaynakları öncelik sırasıyla (AFAD > Kandilli > EMSC) yazar.
func sourcesLine(srcs []string) string {
	s := append([]string(nil), srcs...)
	sort.SliceStable(s, func(i, j int) bool {
		pi, iok := normalize.SourcePriority[s[i]]
		pj, jok := normalize.SourcePriority[s[j]]
		if iok != jok {
			return iok
		}
		return pi < pj
	})
	seen := map[string]bool{}
	var names []string
	for _, src := range s {
		n, ok := sourceNames[src]
		if !ok {
			n = src
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, EscapeHTML(n))
		}
	}
	return strings.Join(names, ", ")
}

// Target, bildirimin gideceği aboneliğin mesaj için gereken kısmıdır.
type Target struct {
	City     string
	Lat, Lon float64
}

// FormatEvent, Plan §4 Senaryo 1'deki bildirim metnini üretir (HTML parse mode).
// prevMag nil değilse bu bir güncelleme bildirimidir ve önceki değer de yazılır.
func FormatEvent(ev normalize.Event, to Target, prevMag *float64) string {
	place := strings.TrimSpace(ev.PlaceName)
	if place == "" {
		c, _ := geo.NearestCity(ev.Lat, ev.Lon, 0)
		place = c.Name + " yakınları"
	}
	magType := strings.TrimSpace(ev.MagType)
	if magType == "" {
		magType = "M"
	}

	var b strings.Builder
	// 1. satır: seviye, yer, büyüklük, saat, derinlik
	b.WriteString(MagnitudeEmoji(ev.Magnitude))
	b.WriteString(" <b>")
	if prevMag != nil {
		b.WriteString("Güncelleme · ")
	}
	fmt.Fprintf(&b, "%s, %s %s</b> · %s", EscapeHTML(place), EscapeHTML(magType),
		FormatDecimal(ev.Magnitude), ev.OriginTime.In(istanbul).Format("15:04"))
	if ev.DepthKm != nil {
		fmt.Fprintf(&b, " · derinlik %s km", formatTrimmed(*ev.DepthKm))
	}
	if prevMag != nil {
		fmt.Fprintf(&b, "\nBüyüklük %s olarak düzeltildi (önceki bildirim: %s).",
			FormatDecimal(ev.Magnitude), FormatDecimal(*prevMag))
	}

	// 2. satır: uzaklık + hissedilme. Kişinin konumunu bilmiyoruz; yalnızca
	// abone olunan il merkezini biliyoruz, bu yüzden "merkezine" diyoruz.
	d := geo.HaversineKm(to.Lat, to.Lon, ev.Lat, ev.Lon)
	km := int(math.Round(d))
	b.WriteString("\n")
	if km < 1 {
		fmt.Fprintf(&b, "%s merkezine çok yakın.", EscapeHTML(to.City))
	} else {
		fmt.Fprintf(&b, "%s merkezine yaklaşık %d km uzaklıkta.", EscapeHTML(to.City), km)
	}
	if p := FeltPhrase(ev.Magnitude, d); p != "" {
		b.WriteString(" " + p)
	}

	// 3. satır: kaynak gösterimi (Plan §17)
	b.WriteString("\n")
	if s := sourcesLine(ev.Sources); s != "" {
		b.WriteString("Kaynaklar: " + s + " · ")
	}
	b.WriteString("Resmî bilgi: AFAD")
	return b.String()
}

// withDisclaimer, bot yanıtlarına kısa eğitim projesi uyarısını ekler.
func withDisclaimer(s string) string { return s + "\n\n" + shortDisclaimer }

func round1(v float64) float64 { return math.Round(v*10) / 10 }
