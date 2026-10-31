package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
)

// SubStore, Notifier'ın veritabanından beklediğidir; testlerde sahtesi verilir.
type SubStore interface {
	AffectedSubscriptions(ctx context.Context, ev normalize.Event) ([]store.Subscription, error)
	LastNotifiedMagnitude(ctx context.Context, eventID string, subID int64) (store.NotifyState, error)
	RecordNotification(ctx context.Context, eventID string, subID int64, kind string, mag float64) (bool, error)
	DeleteNotification(ctx context.Context, eventID string, subID int64, kind string) error
}

// Notifier, events.updated mesajlarını işleyip etkilenen abonelere bildirim gönderir.
type Notifier struct {
	Store SubStore
	// Submit, mesajı hız sınırlı kuyruğa koyar (Queue.Submit).
	Submit func(ctx context.Context, chatID int64, html string) <-chan error
	// MaxAge: bundan eski depremler için bildirim gönderilmez. Kesinti sonrası
	// biriken mesajlar veya replay ile gelen geçmiş olaylar kullanıcıyı
	// "şimdi oldu" sanısıyla yanıltmasın diye.
	MaxAge time.Duration
	Now    func() time.Time
	Log    *slog.Logger
}

type pendingSend struct {
	sub  store.Subscription
	kind string
	done <-chan error
}

// Handle, tek bir olay güncellemesini işler.
//
// Sıra (her abone için): kaydet -> gönder -> başarısızsa kaydı sil.
//   - Kayıt gönderimden ÖNCE yapılır: iki tüketici aynı mesajı aynı anda
//     işlese bile PRIMARY KEY yalnızca birine "gönder" der.
//   - Geçici hata (ağ, 429 tükenmesi) olursa kayıt silinir ve hata döner;
//     mesaj ACK'lenmez, consumer group yeniden teslim eder, yalnızca
//     gönderilemeyenler tekrar denenir (başarılılar artık "duplicate").
//   - Kalıcı hata (403 botu engelledi) kaydı silmez; aksi halde sonsuz döngü.
//   - Kaydettikten sonra süreç çökerse o bildirim kaybolabilir (at-most-once
//     penceresi); iki kez göndermek yerine bu bilinçli olarak seçildi.
func (n *Notifier) Handle(ctx context.Context, upd normalize.EventUpdate) error {
	ev := upd.Event
	log := n.Log.With("event_id", ev.ID, "version", ev.Version, "mag", ev.Magnitude)
	if n.MaxAge > 0 && n.now().Sub(ev.OriginTime) > n.MaxAge {
		log.Debug("eski olay; bildirim yok", "origin_time", ev.OriginTime)
		return nil
	}
	subs, err := n.Store.AffectedSubscriptions(ctx, ev)
	if err != nil {
		return err
	}

	var firstErr error
	keep := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	var pend []pendingSend
	for _, sub := range subs {
		st, err := n.Store.LastNotifiedMagnitude(ctx, ev.ID, sub.ID)
		if err != nil {
			keep(err)
			continue
		}
		kind := Decide(st, ev.Magnitude, sub.MinMag)
		if kind == "" {
			continue
		}
		inserted, err := n.Store.RecordNotification(ctx, ev.ID, sub.ID, kind, ev.Magnitude)
		if err != nil {
			keep(err)
			continue
		}
		if !inserted {
			// Aynı mesaj yeniden teslim edildi ya da başka tüketici gönderdi.
			metrics.NotificationsSent.WithLabelValues("duplicate").Inc()
			continue
		}
		var prev *float64
		if kind == store.KindUpdate {
			p := st.LastMag
			prev = &p
		}
		html := FormatEvent(ev, Target{City: sub.City, Lat: sub.Lat, Lon: sub.Lon}, prev)
		pend = append(pend, pendingSend{sub: sub, kind: kind, done: n.Submit(ctx, sub.ChatID, html)})
	}

	// Mesaj ACK'lenmeden önce tüm gönderimlerin sonucunu bekle.
	for _, p := range pend {
		err := <-p.done
		switch {
		case err == nil:
			metrics.NotificationsSent.WithLabelValues("sent").Inc()
			if p.kind == store.KindInitial && !ev.FirstReportedAt.IsZero() {
				metrics.NotifyLatency.Observe(n.now().Sub(ev.FirstReportedAt).Seconds())
			}
			log.Info("bildirim gönderildi", "subscription_id", p.sub.ID, "kind", p.kind)
		case IsPermanent(err):
			metrics.NotificationsSent.WithLabelValues("error").Inc()
			log.Warn("kalıcı gönderim hatası; yeniden denenmeyecek", "subscription_id", p.sub.ID, "err", err)
		default:
			metrics.NotificationsSent.WithLabelValues("error").Inc()
			log.Warn("gönderim başarısız; kayıt geri alınıyor", "subscription_id", p.sub.ID, "err", err)
			if derr := n.Store.DeleteNotification(context.WithoutCancel(ctx), ev.ID, p.sub.ID, p.kind); derr != nil {
				log.Error("kayıt geri alınamadı; bu bildirim kaybolabilir", "subscription_id", p.sub.ID, "err", derr)
			}
			keep(err)
		}
	}
	if firstErr != nil {
		return errors.Join(errors.New("bazı bildirimler işlenemedi"), firstErr)
	}
	return nil
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}
