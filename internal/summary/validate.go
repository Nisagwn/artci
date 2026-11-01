package summary

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Uzunluk sınırları (rune). 2–3 kısa cümle bu aralığa sığar.
const (
	MinLen       = 40
	MaxLen       = 400
	MaxSentences = 4
)

// Rejection, özetin neden reddedildiğidir. Code, red-team testlerinde ve
// loglarda kullanılan sabit bir etikettir.
type Rejection struct {
	Code   string // schema | length | number | number_word | forbidden | format | missing_magnitude
	Detail string
}

func (r *Rejection) Error() string { return r.Code + ": " + r.Detail }

func reject(code, format string, args ...any) *Rejection {
	return &Rejection{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// ParseModelOutput, modelin ham yanıtını şemaya göre çözer: tam olarak
// {"ozet": "<metin>"}. Kod bloğu çitleri (```json) tolere edilir; başka her
// şey (ek alan, düz metin, boş özet) şema hatasıdır.
func ParseModelOutput(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var out struct {
		Ozet *string `json:"ozet"`
	}
	if err := dec.Decode(&out); err != nil {
		return "", reject("schema", "JSON çözülemedi: %v", err)
	}
	if dec.More() {
		return "", reject("schema", "JSON nesnesinden sonra fazladan içerik")
	}
	if out.Ozet == nil || strings.TrimSpace(*out.Ozet) == "" {
		return "", reject("schema", `"ozet" alanı yok ya da boş`)
	}
	return strings.TrimSpace(*out.Ozet), nil
}

var trLower = cases.Lower(language.Turkish)

// numberToken: 4,1 · 4.1 · 15:04 · 15.04 · 2026 · %20 içindeki 20 ...
var numberToken = regexp.MustCompile(`\d+(?:[.,:]\d+)*`)

// clockToken: iki haneli saat + iki haneli dakika.
var clockToken = regexp.MustCompile(`^(\d{2})[.:](\d{2})$`)

// normNumber, sayı belirtecini karşılaştırılabilir biçime getirir:
// ondalık nokta virgüle çevrilir ("4.1" == "4,1"); saat ayracı ':' korunur
// ama "15.04" de saat olarak "15:04" sayılır (Türkçede iki yazım da yaygın).
func normNumber(s string) string {
	if m := clockToken.FindStringSubmatch(s); m != nil {
		return m[1] + ":" + m[2]
	}
	return strings.ReplaceAll(s, ".", ",")
}

// Yazıyla sayılar: model rakam yerine yazıyla sayı yazarak sayı denetimini
// atlatamasın. "bir" bilerek yok ("bir deprem"); "on" ve "altı" kelime
// olarak geçerse de reddedilir: özet dilinde bu kelimelere ihtiyaç yok.
var numberWords = []string{
	"sıfır", "iki", "üç", "dört", "beş", "altı", "yedi", "sekiz", "dokuz",
	"on", "yirmi", "otuz", "kırk", "elli", "altmış", "yetmiş", "seksen", "doksan",
	"yüz", "bin", "milyon", "buçuk", "yarım", "çeyrek",
	"birkaç", "onlarca", "yüzlerce", "binlerce", "düzinelerce",
}

// forbidden: Plan §11 ve §17. Küçük harfe çevrilmiş metinde alt dize olarak aranır.
var forbidden = []struct{ phrase, why string }{
	// Tahmin
	{"daha büyük", "tahmin"}, {"daha büyüğ", "tahmin"}, {"habercisi", "tahmin"},
	{"öncü deprem", "tahmin"}, {"öncüsü", "tahmin"}, {"beklenen deprem", "tahmin"},
	{"bekleniyor", "tahmin"}, {"beklenmektedir", "tahmin"}, {"tahmin", "tahmin"},
	{"olacak", "tahmin"}, {"olabilecek", "tahmin"}, {"gelecek", "tahmin"}, {"gelebilir", "tahmin"},
	{"sürecek", "tahmin"}, {"devam edecek", "tahmin"}, {"yaklaşıyor", "tahmin"},
	{"tekrarlanabilir", "tahmin"}, {"ihtimal", "tahmin"}, {"olasılığı", "tahmin"},
	{"risk", "tahmin"}, {"fay kırıl", "tahmin"}, {"tetikle", "tahmin"},
	// Panik
	{"felaket", "panik"}, {"yıkım", "panik"}, {"yıkıcı", "panik"}, {"afet", "panik"},
	{"panik", "panik"}, {"korkunç", "panik"}, {"dehşet", "panik"}, {"kıyamet", "panik"},
	{"facia", "panik"}, {"şiddetli", "panik"}, {"büyük deprem", "panik"}, {"tsunami", "panik"},
	{"acil", "panik"}, {"tahliye", "panik"}, {"kaçın", "panik"}, {"dikkat!", "panik"},
	// Doğrulanamayan hasar / kayıp iddiası
	{"yıkıldı", "hasar"}, {"çöktü", "hasar"}, {"hasar", "hasar"}, {"can kaybı", "hasar"},
	{"öldü", "hasar"}, {"ölü", "hasar"}, {"yaralı", "hasar"}, {"enkaz", "hasar"},
	{"hayatını kaybet", "hasar"}, {"göçük", "hasar"}, {"zarar gördü", "hasar"},
	{"çatlak", "hasar"}, {"mahsur", "hasar"},
	// Biçim / enjeksiyon
	{"http", "biçim"}, {"www.", "biçim"}, {"<", "biçim"}, {">", "biçim"},
	{"```", "biçim"}, {"**", "biçim"}, {"#", "biçim"},
}

var wordSplit = regexp.MustCompile(`[\p{L}]+`)

// Validate, özet metnini gerçeklere göre denetler. nil dönerse özet yayınlanabilir.
func Validate(text string, f Facts) error {
	n := utf8.RuneCountInString(text)
	if n < MinLen || n > MaxLen {
		return reject("length", "%d karakter (izin: %d–%d)", n, MinLen, MaxLen)
	}
	if strings.ContainsAny(text, "!?\n\r\t") {
		return reject("format", "ünlem, soru işareti ya da satır sonu içeriyor")
	}
	// Yer adları kaynaktan gelir ve yasak bir kelimeyle başlayabilir
	// ("Ölüdeniz", "On Yedi Eylül") ya da kısaltma noktası içerebilir
	// ("Mah."): kelime ve cümle denetiminden önce metinden çıkarılır.
	lower := trLower.String(text)
	for _, p := range []string{f.Place, f.NearestCity} {
		if p != "" {
			lower = strings.ReplaceAll(lower, trLower.String(p), " ")
		}
	}
	if s := countSentences(lower); s > MaxSentences {
		return reject("length", "%d cümle (en fazla %d)", s, MaxSentences)
	}
	for _, fb := range forbidden {
		if containsAtWordStart(lower, fb.phrase) {
			return reject("forbidden", "%s ifadesi: %q", fb.why, fb.phrase)
		}
	}
	for _, w := range wordSplit.FindAllString(lower, -1) {
		for _, nw := range numberWords {
			if w == nw {
				return reject("number_word", "yazıyla sayı: %q", w)
			}
		}
	}
	allowed := f.AllowedNumbers()
	for _, tok := range numberToken.FindAllString(text, -1) {
		if !allowed[normNumber(tok)] {
			return reject("number", "%q gerçekler nesnesinde yok", tok)
		}
	}
	// Büyüklük özetin özüdür; yoksa özet anlamsızdır.
	if !containsNumber(text, f.Magnitude) {
		return reject("missing_magnitude", "büyüklük (%s) metinde yok", f.Magnitude)
	}
	return nil
}

// containsAtWordStart: harfle başlayan ifadeler yalnızca kelime başında
// eşleşir ("ölü" -> "ölü", "ölümü" evet; "bölümü", "gölü" hayır).
// Simge ifadeleri ("<", "http") her yerde eşleşir.
func containsAtWordStart(s, phrase string) bool {
	first, _ := utf8.DecodeRuneInString(phrase)
	if !unicode.IsLetter(first) {
		return strings.Contains(s, phrase)
	}
	for i := 0; ; {
		j := strings.Index(s[i:], phrase)
		if j < 0 {
			return false
		}
		at := i + j
		prev, _ := utf8.DecodeLastRuneInString(s[:at])
		if at == 0 || !unicode.IsLetter(prev) {
			return true
		}
		i = at + len(phrase)
	}
}

func containsNumber(text, want string) bool {
	w := normNumber(want)
	for _, tok := range numberToken.FindAllString(text, -1) {
		if normNumber(tok) == w {
			return true
		}
	}
	return false
}

// countSentences: sayı içindeki noktalar (4.1, 15.04) cümle sonu sayılmaz.
func countSentences(s string) int {
	n := 0
	rs := []rune(s)
	for i, r := range rs {
		if r != '.' {
			continue
		}
		if i > 0 && i+1 < len(rs) && isDigit(rs[i-1]) && isDigit(rs[i+1]) {
			continue
		}
		n++
	}
	if n == 0 {
		return 1
	}
	return n
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// IsRejection, hatanın doğrulama reddi olup olmadığını söyler.
func IsRejection(err error) (*Rejection, bool) {
	var r *Rejection
	ok := errors.As(err, &r)
	return r, ok
}
