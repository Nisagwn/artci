package notify

import (
	"strconv"
	"strings"
)

// FormatDecimal, sayıyı tek ondalıkla ve Türkçe virgülle yazar (4.1 -> "4,1").
func FormatDecimal(v float64) string {
	return strings.Replace(strconv.FormatFloat(v, 'f', 1, 64), ".", ",", 1)
}

// formatTrimmed, ondalık kısmı sıfırsa atar (10.0 -> "10", 7.4 -> "7,4").
func formatTrimmed(v float64) string {
	s := FormatDecimal(v)
	return strings.TrimSuffix(s, ",0")
}

// htmlEscaper: Telegram HTML modunda yalnızca bu üç karakterin kaçışı gerekir;
// kaynaktan gelen yer adı ("<" içerebilir) mesajı bozmasın diye kullanılır.
var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeHTML, metni Telegram HTML parse modu için güvenli hâle getirir.
func EscapeHTML(s string) string { return htmlEscaper.Replace(s) }
