// notifier: events.updated akışını tüketip etkilenen abonelere Telegram
// bildirimi gönderir ve abonelik komutlarını yanıtlar (Plan §10).
//
// Ortam değişkenleri (ortak olanlar internal/app'te):
//
//	ARTCI_TELEGRAM_TOKEN     bot token'ı; yoksa bot kapalı, bildirimler yalnızca loglanır (dry-run)
//	ARTCI_TELEGRAM_URL       Bot API adresi (varsayılan https://api.telegram.org)
//	ARTCI_NOTIFY_MAX_AGE     bundan eski depremler bildirilmez (varsayılan 1h)
//	ARTCI_NOTIFY_GLOBAL_RATE genel gönderim hızı, msg/sn (varsayılan 25)
//	ARTCI_NOTIFY_CHAT_RATE   sohbet başına gönderim hızı, msg/sn (varsayılan 1)
package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nisaguven/artci/internal/app"
	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/notify"
	"github.com/nisaguven/artci/internal/store"
	"github.com/nisaguven/artci/internal/stream"
	"github.com/nisaguven/artci/migrations"
)

func main() {
	ctx, stop := app.Init("notifier")
	defer stop()
	cfg := app.LoadConfig(":9103")
	metrics.Serve(ctx, cfg.MetricsAddr)

	db, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		app.Fatal("postgres", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		app.Fatal("migrate", err)
	}
	rdb, err := app.Redis(ctx, cfg.RedisAddr)
	if err != nil {
		app.Fatal("redis", err)
	}
	defer rdb.Close()

	token := app.Env("ARTCI_TELEGRAM_TOKEN", "")
	var tg *notify.Telegram
	send := notify.SendFunc(func(_ context.Context, chatID int64, html string) error {
		slog.Info("dry-run: bildirim gönderilmedi (ARTCI_TELEGRAM_TOKEN yok)", "chat_id", chatID, "text", html)
		return nil
	})
	if token != "" {
		tg = notify.NewTelegram(app.Env("ARTCI_TELEGRAM_URL", notify.DefaultTelegramURL), token)
		send = tg.SendMessage
	} else {
		slog.Warn("ARTCI_TELEGRAM_TOKEN yok: bot kapalı, tüketici dry-run modunda")
	}

	limiter := notify.NewLimiter(
		app.EnvFloat("ARTCI_NOTIFY_GLOBAL_RATE", notify.DefaultGlobalRate),
		app.EnvFloat("ARTCI_NOTIFY_CHAT_RATE", notify.DefaultChatRate),
	)
	queue := notify.NewQueue(send, limiter, 4, 256)

	n := &notify.Notifier{
		Store:  db,
		Submit: queue.Submit,
		MaxAge: app.EnvDuration("ARTCI_NOTIFY_MAX_AGE", notify.DefaultMaxAge),
		Log:    slog.Default(),
	}

	var wg sync.WaitGroup
	if tg != nil {
		bot := &notify.Bot{Updates: tg, Store: db, Send: queue.Send, Log: slog.Default()}
		wg.Go(func() {
			slog.Info("bot komut döngüsü başladı")
			bot.Run(ctx)
		})
	}

	consumer := &stream.Consumer{
		RDB: rdb, Stream: stream.EventsUpdated, Group: "notifier", Name: app.Hostname(),
	}
	wg.Go(func() {
		slog.Info("events.updated tüketicisi başladı")
		err := consumer.Run(ctx, func(ctx context.Context, m stream.Message) error {
			var upd normalize.EventUpdate
			if err := m.Decode(&upd); err != nil {
				// Bozuk mesaj yeniden denense de düzelmez: logla ve ACK'le.
				slog.Error("çözülemeyen olay mesajı atlandı", "id", m.ID, "err", err)
				return nil
			}
			return n.Handle(ctx, upd)
		})
		if err != nil {
			slog.Error("tüketici durdu", "err", err)
			stop() // bot da kapansın; süreç yeniden başlatılsın
		}
	})

	// Graceful shutdown: önce tüketici ve bot elindeki işi bitirir, sonra
	// kuyruktaki mesajlar gönderilir; ancak ondan sonra bağlantılar kapanır.
	wg.Wait()
	queue.Close()
	slog.Info("notifier kapandı")
}
