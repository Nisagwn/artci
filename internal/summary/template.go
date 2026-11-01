package summary

import (
	"strings"
)

// Template, LLM olmadan ya da LLM reddedildiğinde kullanılan deterministik
// özettir (Plan §11 madde 4). Yalnızca Facts'teki değerleri kullanır; bu
// yüzden doğrulayıcıdan her zaman geçmelidir (testle güvence altında).
//
//	Sındırgı (Balıkesir) merkezli 4,1 büyüklüğünde bir deprem 30 Ekim 2026
//	saat 15:04'te meydana geldi. Derinlik 7 km. En yakın il merkezi Balıkesir,
//	yaklaşık 50 km uzaklıkta. Veriler: AFAD, Kandilli.
func Template(f Facts) string {
	var b strings.Builder
	b.WriteString(f.Place)
	b.WriteString(" merkezli ")
	b.WriteString(f.Magnitude)
	b.WriteString(" büyüklüğünde bir deprem ")
	b.WriteString(f.Date)
	b.WriteString(" saat ")
	b.WriteString(f.Time)
	b.WriteString(LocativeSuffix(f.Time))
	b.WriteString(" meydana geldi.")
	if f.DepthKm != "" {
		b.WriteString(" Derinlik ")
		b.WriteString(f.DepthKm)
		b.WriteString(" km.")
	}
	if f.NearestCity != "" && !strings.HasPrefix(f.Place, f.NearestCity+" yakınları") {
		b.WriteString(" En yakın il merkezi ")
		b.WriteString(f.NearestCity)
		b.WriteString(", yaklaşık ")
		b.WriteString(f.DistanceKm)
		b.WriteString(" km uzaklıkta.")
	}
	if len(f.Sources) > 0 {
		b.WriteString(" Veriler: ")
		b.WriteString(strings.Join(f.Sources, ", "))
		b.WriteString(".")
	}
	return b.String()
}

// Okunuşun son kelimesine göre bulunma eki. Rakamla yazılan saatte ek,
// sayının okunuşuna uyar: "15:04'te" (dört), "15:06'da" (altı),
// "15:00'te" (on beş), "12:00'de" (on iki).
var (
	onesSuffix = map[byte]string{'1': "'de", '2': "'de", '3': "'te", '4': "'te", '5': "'te",
		'6': "'da", '7': "'de", '8': "'de", '9': "'da"}
	tensSuffix = map[byte]string{'1': "'da", '2': "'de", '3': "'da", '4': "'ta", '5': "'te"}
)

// LocativeSuffix, "SS:DD" biçimindeki saat için bulunma ekini döner.
func LocativeSuffix(clock string) string {
	num := clock
	if i := strings.IndexByte(clock, ':'); i >= 0 {
		num = clock[i+1:]
		if strings.Trim(num, "0") == "" { // tam saat: saatin okunuşu
			num = clock[:i]
		}
	}
	num = strings.TrimLeft(num, "0")
	if num == "" {
		return "'da" // sıfır
	}
	last := num[len(num)-1]
	if last != '0' {
		return onesSuffix[last]
	}
	if len(num) >= 2 {
		return tensSuffix[num[len(num)-2]]
	}
	return "'da"
}
