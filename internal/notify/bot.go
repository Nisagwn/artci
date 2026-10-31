package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nisaguven/artci/internal/store"
)

// MaxSubscriptionsPerChat: kötüye kullanımı ve gereksiz kişisel veriyi sınırlar.
const MaxSubscriptionsPerChat = 5

// BotStore, botun veritabanından beklediğidir.
type BotStore interface {
	AddSubscription(ctx context.Context, sub store.Subscription) (store.Subscription, bool, error)
	ListSubscriptions(ctx context.Context, chatID int64) ([]store.Subscription, error)
	DeleteSubscriptions(ctx context.Context, chatID int64) (int64, error)
}

// Updater, getUpdates kaynağıdır (Telegram ya da test sahtesi).
type Updater interface {
	GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error)
}

// Bot, abonelik komutlarını işleyen long polling döngüsüdür.
type Bot struct {
	Updates Updater
	Store   BotStore
	// Send, yanıtı gönderir; bildirimlerle aynı hız sınırlı kuyruk kullanılır.
	Send func(ctx context.Context, chatID int64, html string) error
	Log  *slog.Logger
}

const startText = "<b>Artçı</b> deprem bildirim botuna hoş geldiniz.\n\n" +
	"Seçtiğiniz ilin çevresindeki depremler için kısa bir bildirim alırsınız. " +
	"Konumunuz değil, yalnızca seçtiğiniz ilin merkezi saklanır; /iptal ile tüm kayıtlarınız silinir.\n\n" +
	helpBody + "\n\n⚠️ " + Disclaimer

const helpBody = "<b>Komutlar</b>\n" +
	"/abone &lt;il&gt; &lt;yarıçap_km&gt; &lt;min_büyüklük&gt;: ör. <code>/abone Balıkesir 100 3,5</code>\n" +
	"   (yarıçap 10-500 km, varsayılan 100; büyüklük varsayılan 3,5)\n" +
	"/aboneliklerim: aboneliklerinizi listeler\n" +
	"/iptal: tüm aboneliklerinizi ve kayıtlarınızı siler\n" +
	"/yardim: bu mesaj"

const tryLaterText = "Şu an işlem yapılamadı, lütfen biraz sonra tekrar deneyin."

// Reply, tek bir mesaja verilecek yanıtı üretir (HTML). Veritabanı dışında yan etkisi yoktur.
func (b *Bot) Reply(ctx context.Context, chatID int64, text string) string {
	cmd, err := ParseCommand(text)
	var ue *UserError
	switch {
	case errors.Is(err, ErrNotCommand):
		return withDisclaimer("Komutlarla çalışıyorum. Başlamak için /yardim yazın.")
	case errors.As(err, &ue):
		return withDisclaimer(EscapeHTML(ue.Msg))
	case err != nil:
		return withDisclaimer(tryLaterText)
	}

	switch cmd.Name {
	case CmdStart:
		return startText // tam uyarı zaten içinde
	case CmdHelp:
		return withDisclaimer(helpBody + "\n\n" + Disclaimer)
	case CmdSubscribe:
		return withDisclaimer(b.subscribe(ctx, chatID, cmd))
	case CmdSubscriptions:
		subs, err := b.Store.ListSubscriptions(ctx, chatID)
		if err != nil {
			b.Log.Error("abonelikler okunamadı", "err", err)
			return withDisclaimer(tryLaterText)
		}
		return withDisclaimer(FormatSubscriptions(subs))
	case CmdCancel:
		n, err := b.Store.DeleteSubscriptions(ctx, chatID)
		if err != nil {
			b.Log.Error("abonelikler silinemedi", "err", err)
			return withDisclaimer(tryLaterText)
		}
		if n == 0 {
			return withDisclaimer("Kayıtlı aboneliğiniz yok.")
		}
		return withDisclaimer(fmt.Sprintf("%d aboneliğiniz ve ilgili tüm kayıtlar silindi. Artık bildirim almayacaksınız.", n))
	}
	return withDisclaimer(tryLaterText)
}

func (b *Bot) subscribe(ctx context.Context, chatID int64, cmd Command) string {
	subs, err := b.Store.ListSubscriptions(ctx, chatID)
	if err != nil {
		b.Log.Error("abonelikler okunamadı", "err", err)
		return tryLaterText
	}
	exists := false
	for _, s := range subs {
		exists = exists || s.City == cmd.City.Name
	}
	if !exists && len(subs) >= MaxSubscriptionsPerChat {
		return fmt.Sprintf("En fazla %d abonelik ekleyebilirsiniz. /aboneliklerim ile bakıp /iptal ile temizleyebilirsiniz.", MaxSubscriptionsPerChat)
	}
	_, updated, err := b.Store.AddSubscription(ctx, store.Subscription{
		ChatID: chatID, City: cmd.City.Name, Lat: cmd.City.Lat, Lon: cmd.City.Lon,
		RadiusKm: cmd.RadiusKm, MinMag: cmd.MinMag,
	})
	if err != nil {
		b.Log.Error("abonelik eklenemedi", "err", err)
		return tryLaterText
	}
	verb := "Abonelik eklendi"
	if updated {
		verb = "Abonelik güncellendi"
	}
	return fmt.Sprintf("✅ %s: <b>%s</b> merkezinin %d km çevresindeki %s ve üzeri depremler.",
		verb, EscapeHTML(cmd.City.Name), cmd.RadiusKm, FormatDecimal(cmd.MinMag))
}

// FormatSubscriptions, /aboneliklerim yanıtıdır.
func FormatSubscriptions(subs []store.Subscription) string {
	if len(subs) == 0 {
		return "Henüz aboneliğiniz yok. Örnek: <code>/abone Balıkesir 100 3,5</code>"
	}
	var sb strings.Builder
	sb.WriteString("<b>Abonelikleriniz</b>")
	for _, s := range subs {
		fmt.Fprintf(&sb, "\n• %s: %d km, %s ve üzeri", EscapeHTML(s.City), s.RadiusKm, FormatDecimal(s.MinMag))
	}
	return sb.String()
}

// Run, ctx iptal edilene kadar güncellemeleri alıp yanıtlar.
// offset bellekte tutulur: yeniden başlatmada son birkaç komut tekrar
// işlenebilir; komutlar idempotent olduğu için (aynı il = güncelleme) zararsızdır.
func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		ups, err := b.Updates.GetUpdates(ctx, offset, 25)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			b.Log.Warn("getUpdates başarısız", "err", err, "retry_in", backoff)
			if sleepCtx(ctx, backoff) != nil {
				break
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			offset = max(offset, u.UpdateID+1)
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			// Yanıtı kapanışta yarıda kesmemek için iptal edilmeyen bağlam.
			work := context.WithoutCancel(ctx)
			reply := b.Reply(work, u.Message.Chat.ID, u.Message.Text)
			if err := b.Send(work, u.Message.Chat.ID, reply); err != nil {
				b.Log.Warn("yanıt gönderilemedi", "err", err)
			}
		}
	}
	return nil
}
