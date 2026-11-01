package source

import (
	"context"
	"log/slog"
	"time"

	"github.com/nisaguven/artci/internal/metrics"
)

// Poller, bir Fetcher'ı düzenli aralıklarla çalıştırır (AFAD, Kandilli).
//
//   - Başarılı turdan sonra Interval (±%10 jitter) bekler.
//   - Hata olursa Backoff ile artan sürelerle yeniden dener.
//   - Breaker açılırsa Cooldown boyunca kaynağa hiç istek atmaz; diğer
//     kaynaklar ayrı süreçlerde çalıştığı için etkilenmez.
type Poller struct {
	Fetcher  Fetcher
	Emitter  *Emitter
	Interval time.Duration
	Backoff  Backoff
	Breaker  *Breaker
}

// Run, ctx iptal edilene kadar çalışır. Graceful shutdown: süren istek ctx ile
// iptal edilir; yayınlanmış raporlar zaten akıştadır, yarım kalan iş olmaz.
func (p *Poller) Run(ctx context.Context) {
	name := p.Fetcher.Name()
	if p.Interval <= 0 {
		p.Interval = 30 * time.Second
	}
	if p.Breaker == nil {
		p.Breaker = &Breaker{Threshold: 5, Cooldown: 5 * time.Minute}
	}
	failures := 0
	for ctx.Err() == nil {
		if ok, wait := p.Breaker.Allow(); !ok {
			metrics.SourceUp.WithLabelValues(name).Set(0)
			sleep(ctx, wait)
			continue
		}
		wait := p.tick(ctx, name, &failures)
		sleep(ctx, wait)
	}
	slog.Info("toplayıcı durdu", "source", name)
}

// tick, tek bir turu çalıştırır ve sonraki tura kadar beklenecek süreyi döner.
func (p *Poller) tick(ctx context.Context, name string, failures *int) time.Duration {
	start := time.Now()
	res, err := p.Fetcher.Fetch(ctx)
	if ctx.Err() != nil {
		return 0
	}
	if err != nil {
		*failures++
		metrics.SourceUp.WithLabelValues(name).Set(0)
		opened := p.Breaker.Failure()
		wait := p.Backoff.Next(*failures)
		slog.Warn("kaynak okunamadı", "source", name, "err", err, "attempt", *failures,
			"retry_in", wait.Round(time.Millisecond), "breaker_opened", opened)
		return wait
	}
	*failures = 0
	p.Breaker.Success()
	metrics.SourceUp.WithLabelValues(name).Set(1)
	metrics.SourceLastSuccess.WithLabelValues(name).SetToCurrentTime()

	st, err := p.Emitter.Emit(ctx, res)
	if err != nil {
		// Kaynak sağlam ama Redis'e yazamadık: yayınlanamayanlar önbelleğe
		// işlenmedi, sonraki turda yeniden denenecek. Kısa bir bekleme yeterli.
		slog.Error("yayın hatası; sonraki turda yeniden denenecek", "source", name, "err", err)
		return p.Backoff.Next(1)
	}
	if st.Published > 0 || st.DLQ > 0 {
		slog.Info("tur tamam", "source", name, "published", st.Published, "skipped", st.Skipped,
			"dlq", st.DLQ, "took", time.Since(start).Round(time.Millisecond))
	}
	return Jitter(p.Interval, 0.1, nil)
}
