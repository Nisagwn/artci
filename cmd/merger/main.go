// Command merger, reports.raw akışındaki raporları tekilleştirip olaylara
// dönüştürür ve değişen olayları events.updated akışına yazar (Plan §6, §8).
//
// Ortam değişkenleri (app.LoadConfig'e ek olarak):
//
//	ARTCI_MERGER_CONSUMER   tüketici adı (varsayılan "merger-1")
//	ARTCI_MATCH_WINDOW      aday zaman penceresi (varsayılan 60s)
//	ARTCI_MATCH_RADIUS_KM   aday yarıçapı (varsayılan 100)
//	ARTCI_MATCH_MIN_SCORE   bağlama eşiği (varsayılan 0.6)
package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nisaguven/artci/internal/app"
	"github.com/nisaguven/artci/internal/match"
	"github.com/nisaguven/artci/internal/merger"
	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
	"github.com/nisaguven/artci/internal/stream"
	"github.com/nisaguven/artci/migrations"
)

func main() {
	ctx, stop := app.Init("merger")
	defer stop()
	cfg := app.LoadConfig(":9102")

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

	mc := match.DefaultConfig()
	mc.TimeWindow = app.EnvDuration("ARTCI_MATCH_WINDOW", mc.TimeWindow)
	mc.MaxDistanceKm = app.EnvFloat("ARTCI_MATCH_RADIUS_KM", mc.MaxDistanceKm)
	mc.MinScore = app.EnvFloat("ARTCI_MATCH_MIN_SCORE", mc.MinScore)
	svc := merger.New(merger.PGStore{S: st}, merger.StreamPublisher{RDB: rdb}, mc)

	// Önceki çalışmada commit edilip yayınlanamamış olaylar varsa önce onlar.
	if err := svc.PublishPending(ctx); err != nil {
		slog.Warn("açılışta bekleyen yayınlar tamamlanamadı", "err", err)
	}

	// Tek tüketici (Plan §6 "neden tek merger"). Ad sabit: konteyner yeniden
	// başlayınca hostname değişse bile aynı ad, çöküşten kalan bekleyen
	// mesajları hemen geri alır (drainOwnPending).
	c := stream.Consumer{
		RDB:    rdb,
		Stream: stream.ReportsRaw,
		Group:  "merger",
		Name:   app.Env("ARTCI_MERGER_CONSUMER", "merger-1"),
	}
	slog.Info("merger başladı", "consumer", c.Name, "min_score", mc.MinScore)
	if err := c.Run(ctx, handler(svc, rdb)); err != nil {
		app.Fatal("consumer", err)
	}
	slog.Info("merger kapandı")
}

// handler, hata türüne göre ACK kararını verir:
//   - çözülemeyen / geçersiz / kalıcı DB hatası -> DLQ + ACK (nil döner),
//     yoksa zehirli mesaj sonsuza dek yeniden denenirdi;
//   - geçici hata (DB/Redis erişilemiyor) -> ACK yok, mesaj sonra yeniden işlenir.
func handler(svc *merger.Service, rdb *redis.Client) stream.Handler {
	return func(ctx context.Context, m stream.Message) error {
		var r normalize.Report
		if err := m.Decode(&r); err != nil {
			return toDLQ(ctx, rdb, "unknown", "decode: "+err.Error(), m)
		}
		if err := r.Validate(); err != nil {
			return toDLQ(ctx, rdb, r.Source, "validate: "+err.Error(), m)
		}
		dec, err := svc.Handle(ctx, r)
		switch {
		case err == nil:
			slog.Debug("rapor işlendi", "decision", dec, "source", r.Source, "id", r.SourceEventID)
			return nil
		case merger.IsPermanent(err):
			return toDLQ(ctx, rdb, r.Source, "db: "+err.Error(), m)
		default:
			slog.Error("geçici hata; mesaj yeniden denenecek", "err", err, "msg_id", m.ID,
				"publish_only", errors.Is(err, merger.ErrPublish))
			// Kısa bekleme: DB çökükken sıradaki mesajlarda boşa dönmeyelim.
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			return err
		}
	}
}

func toDLQ(ctx context.Context, rdb *redis.Client, source, reason string, m stream.Message) error {
	slog.Warn("mesaj DLQ'ya gönderildi", "source", source, "reason", reason, "msg_id", m.ID)
	if err := stream.PublishDLQ(ctx, rdb, source, reason, string(m.Data)); err != nil {
		return err // DLQ'ya da yazamadık: ACK'leme, kaybolmasın
	}
	metrics.DLQMessages.WithLabelValues(source).Inc()
	return nil
}
