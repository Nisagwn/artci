// Command summarizer, events.updated akışını tüketip M ≥ 3,5 olaylar için
// doğrulanmış kısa Türkçe özet üretir (Plan §11).
//
// LLM yapılandırılmamışsa (ARTCI_LLM_API_KEY yok) yalnızca şablonlu özet yazar;
// hat LLM'siz de eksiksiz çalışır.
//
// Ortam değişkenleri (ortak olanlar internal/app'te):
//
//	ARTCI_LLM_API_KEY        sağlayıcı anahtarı; yoksa yalnızca şablon
//	ARTCI_LLM_URL            OpenAI uyumlu sohbet ucu (varsayılan OpenRouter)
//	ARTCI_LLM_MODEL          model adı
//	ARTCI_LLM_PRICE_IN       USD / 1M girdi token'ı (sağlayıcı maliyet döndürmezse)
//	ARTCI_LLM_PRICE_OUT      USD / 1M çıktı token'ı
//	ARTCI_LLM_DAILY_USD      günlük harcama tavanı (varsayılan 1)
//	ARTCI_LLM_TIMEOUT        tek çağrı zaman aşımı (varsayılan 15s)
//	ARTCI_SUMMARY_MIN_MAG    özet eşiği (varsayılan 3,5)
//	ARTCI_SUMMARIZER_CONSUMER tüketici adı (varsayılan "summarizer-1")
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/nisaguven/artci/internal/app"
	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
	"github.com/nisaguven/artci/internal/stream"
	"github.com/nisaguven/artci/internal/summary"
	"github.com/nisaguven/artci/migrations"
)

func main() {
	ctx, stop := app.Init("summarizer")
	defer stop()
	cfg := app.LoadConfig(":9104")

	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		app.Fatal("postgres", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, migrations.FS); err != nil {
		app.Fatal("migrate", err)
	}
	rdb, err := app.Redis(ctx, cfg.RedisAddr)
	if err != nil {
		app.Fatal("redis", err)
	}
	defer rdb.Close()
	metrics.Serve(ctx, cfg.MetricsAddr)

	sc := summary.DefaultConfig()
	sc.MinMagnitude = app.EnvFloat("ARTCI_SUMMARY_MIN_MAG", sc.MinMagnitude)
	sc.DailyCapUSD = app.EnvFloat("ARTCI_LLM_DAILY_USD", sc.DailyCapUSD)
	sc.LLMTimeout = app.EnvDuration("ARTCI_LLM_TIMEOUT", sc.LLMTimeout)
	svc := &summary.Service{Store: st, Cfg: sc}

	if key := app.Env("ARTCI_LLM_API_KEY", ""); key != "" {
		svc.LLM = &summary.OpenAICompatible{
			URL:        app.Env("ARTCI_LLM_URL", "https://openrouter.ai/api/v1/chat/completions"),
			APIKey:     key,
			Model:      app.Env("ARTCI_LLM_MODEL", "anthropic/claude-haiku-5.5"),
			InputPerM:  app.EnvFloat("ARTCI_LLM_PRICE_IN", 0),
			OutputPerM: app.EnvFloat("ARTCI_LLM_PRICE_OUT", 0),
		}
		slog.Info("LLM etkin", "generator", svc.LLM.Name(), "daily_cap_usd", sc.DailyCapUSD)
	} else {
		slog.Info("ARTCI_LLM_API_KEY yok; yalnızca şablonlu özet")
	}

	c := stream.Consumer{
		RDB:    rdb,
		Stream: stream.EventsUpdated,
		Group:  "summarizer",
		Name:   app.Env("ARTCI_SUMMARIZER_CONSUMER", "summarizer-1"),
		// LLM çağrısı 15 sn sürebilir; başkası devralmasın diye geniş tutulur.
		ClaimIdle: 2 * time.Minute,
	}
	slog.Info("summarizer başladı", "consumer", c.Name, "min_mag", sc.MinMagnitude)
	err = c.Run(ctx, func(ctx context.Context, m stream.Message) error {
		var u normalize.EventUpdate
		if err := m.Decode(&u); err != nil || u.Event.ID == "" {
			slog.Warn("events.updated çözülemedi; atlandı", "msg_id", m.ID, "err", err)
			return nil // yeniden denemek işe yaramaz
		}
		out, err := svc.Handle(ctx, u)
		if err != nil {
			slog.Error("geçici hata; mesaj yeniden denenecek", "err", err, "event_id", u.Event.ID)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			return err
		}
		if out == summary.OutcomeLLM || out == summary.OutcomeFallback {
			slog.Info("özet yazıldı", "event_id", u.Event.ID, "version", u.Event.Version, "outcome", out)
		}
		return nil
	})
	if err != nil {
		app.Fatal("consumer", err)
	}
	slog.Info("summarizer kapandı")
}
