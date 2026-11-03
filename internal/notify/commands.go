// Package notify, Telegram botu ve bildirim gönderimidir (Plan §10).
//
// Saf parçalar (komut ayrıştırma, mesaj biçimlendirme, karar mantığı, hız
// sınırlayıcı) ağdan ve veritabanından bağımsızdır; böylece tablo testleriyle
// sınanabilir. Yan etkili parçalar (Telegram istemcisi, bot döngüsü, Notifier)
// bunları birleştirir.
package notify

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/nisaguven/artci/internal/geo"
)

// Komut adları.
const (
	CmdStart         = "start"
	CmdHelp          = "yardim"
	CmdSubscribe     = "abone"
	CmdSubscriptions = "aboneliklerim"
	CmdCancel        = "iptal"
)

// Abonelik sınırları. Yarıçap sınırı şemadaki CHECK ile aynıdır (Plan §7);
// büyüklük sınırı ise anlamsız eşikleri (ör. 0,1 veya 12) baştan reddetmek içindir.
const (
	DefaultRadiusKm = 100
	DefaultMinMag   = 3.5
	MinRadiusKm     = 10
	MaxRadiusKm     = 500
	MinMinMag       = 2.0
	MaxMinMag       = 9.0
)

// Command, ayrıştırılmış bir bot komutudur.
type Command struct {
	Name     string
	City     geo.City // yalnızca /abone
	RadiusKm int      // yalnızca /abone
	MinMag   float64  // yalnızca /abone
}

// UserError, kullanıcıya olduğu gibi gösterilebilecek bir hatadır.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// ErrNotCommand: mesaj boş; bot yalnızca kısa bir ipucu döner.
var ErrNotCommand = errors.New("komut değil")

// Menü düğmeleri: mesaj kutusunun altında durur, basılınca metni sohbete gönderilir.
const (
	BtnSubscriptions = "📋 Aboneliklerim"
	BtnCancel        = "🗑 Aboneliği bitir"
	BtnHelp          = "❓ Yardım"
)

// MenuKeyboard, bot yanıtlarına eklenen kalıcı düğme düzenidir.
var MenuKeyboard = [][]string{{BtnSubscriptions, BtnCancel}, {BtnHelp}}

// ParseCommand, bir Telegram mesajını komuta çevirir. Hatalar *UserError ise
// metni doğrudan kullanıcıya gösterilir.
//
// Komut ezberlemek gerekmesin diye "/" ile başlamayan metin de anlaşılır:
// menü düğmeleri ve selamlar ilgili komuta, geri kalanı il adına ("İzmir",
// isteğe bağlı olarak "İzmir 50 3") çevrilir.
func ParseCommand(text string) (Command, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return Command{}, ErrNotCommand
	}
	if !strings.HasPrefix(fields[0], "/") {
		if name, ok := plainCommand(text); ok {
			return Command{Name: name}, nil
		}
		if foldCmd(fields[0]) == "abone" { // "abone İzmir" da olur
			fields = fields[1:]
		}
		return parseSubscribe(fields)
	}
	name := strings.TrimPrefix(fields[0], "/")
	// Gruplarda Telegram komutu "/abone@ArtciBot" biçiminde gönderir.
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	name = foldCmd(name)
	args := fields[1:]

	switch name {
	case "start":
		return Command{Name: CmdStart}, nil
	case "yardim", "help":
		return Command{Name: CmdHelp}, nil
	case "aboneliklerim", "liste":
		return Command{Name: CmdSubscriptions}, nil
	case "iptal":
		return Command{Name: CmdCancel}, nil
	case "abone":
		return parseSubscribe(args)
	default:
		return Command{}, userErr("Bu komutu tanımıyorum. Komutlar için /yardim yazın.")
	}
}

// plainCommand, düğme metinlerini ve selamları komuta çevirir. Baştaki emoji
// atılır; böylece "Aboneliklerim" elle yazılsa da çalışır.
func plainCommand(text string) (string, bool) {
	s := foldCmd(strings.TrimLeftFunc(strings.TrimSpace(text), func(r rune) bool { return !unicode.IsLetter(r) }))
	switch s {
	case "aboneliklerim", "liste":
		return CmdSubscriptions, true
	case "aboneligi bitir", "iptal":
		return CmdCancel, true
	case "yardim", "help":
		return CmdHelp, true
	case "merhaba", "selam", "basla", "start":
		return CmdStart, true
	}
	return "", false
}

// foldCmd, "/Yardım" ve "/yardim" gibi yazımları eşitler.
func foldCmd(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("ı", "i", "İ", "i", "i̇", "i", "ğ", "g", "ü", "u", "ş", "s", "ö", "o", "ç", "c").Replace(s)
}

// parseSubscribe: /abone <şehir> [yarıçap_km] [min_büyüklük].
// Şehir birden çok kelime olabilir; sondaki sayısal argümanlar sırasıyla
// yarıçap ve büyüklüktür. Böylece "/abone Balıkesir" varsayılanlarla çalışır.
func parseSubscribe(args []string) (Command, error) {
	if len(args) == 0 {
		return Command{}, userErr("Hangi il? Sadece il adını yazın, ör. İzmir")
	}
	// Sondan en fazla iki sayısal argümanı ayır.
	nums := 0
	for i := len(args) - 1; i >= 1 && nums < 2; i-- {
		if !looksNumeric(args[i]) {
			break
		}
		nums++
	}
	cityArgs, numArgs := args[:len(args)-nums], args[len(args)-nums:]
	cityName := strings.Join(cityArgs, " ")
	// Kalan argümanlarda sayı varsa sıra bozuktur (ör. "/abone 100 Balıkesir").
	for _, a := range cityArgs {
		if looksNumeric(a) {
			return Command{}, userErr("Önce il adını yazın, ör. İzmir 50 3")
		}
	}
	city, ok := geo.FindCity(cityName)
	if !ok {
		return Command{}, userErr("“%s” adında bir il bulamadım. Sadece il adını yazın, ör. İzmir", cityName)
	}

	cmd := Command{Name: CmdSubscribe, City: city, RadiusKm: DefaultRadiusKm, MinMag: DefaultMinMag}
	if len(numArgs) >= 1 {
		r, err := parseRadius(numArgs[0])
		if err != nil {
			return Command{}, err
		}
		cmd.RadiusKm = r
	}
	if len(numArgs) == 2 {
		m, err := parseMag(numArgs[1])
		if err != nil {
			return Command{}, err
		}
		cmd.MinMag = m
	}
	return cmd, nil
}

// looksNumeric: rakamla başlayan argüman sayı kabul edilir ("100", "3,5", "100km").
func looksNumeric(s string) bool {
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

// parseNumber, Türkçe virgüllü ondalığı da kabul eder ("3,5" == "3.5").
func parseNumber(s string) (float64, error) {
	return strconv.ParseFloat(strings.Replace(s, ",", ".", 1), 64)
}

func parseRadius(s string) (int, error) {
	s = strings.TrimSuffix(strings.ToLower(s), "km")
	v, err := parseNumber(s)
	if err != nil || v != math.Trunc(v) {
		return 0, userErr("Yarıçap tam sayı (km) olmalı, ör. 100.")
	}
	if v < MinRadiusKm || v > MaxRadiusKm {
		return 0, userErr("Yarıçap %d ile %d km arasında olmalı.", MinRadiusKm, MaxRadiusKm)
	}
	return int(v), nil
}

func parseMag(s string) (float64, error) {
	v, err := parseNumber(s)
	if err != nil || math.IsNaN(v) {
		return 0, userErr("En küçük büyüklük bir sayı olmalı, ör. 3,5.")
	}
	// Şema NUMERIC(3,1): tek ondalığa yuvarla ki kullanıcıya gösterilen ile saklanan aynı olsun.
	v = math.Round(v*10) / 10
	if v < MinMinMag || v > MaxMinMag {
		return 0, userErr("En küçük büyüklük %s ile %s arasında olmalı.", FormatDecimal(MinMinMag), FormatDecimal(MaxMinMag))
	}
	return v, nil
}
