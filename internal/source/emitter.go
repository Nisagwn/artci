package source

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
)

// Emitter, kaynaktan okunan sonuçları Sink'e iletir ve gereksiz tekrarları süzer.
//
// Polling kaynakları her turda son N depremi yeniden döndürür. Bunları her
// seferinde yayınlamak doğru olurdu (merger idempotent) ama akışı ve DB'yi
// boşuna meşgul ederdi. Emitter, her raporun anlamlı alanlarının özetini
// tutar ve yalnızca yeni ya da değişmiş (revize) raporları yayınlar.
// Bu bir optimizasyondur; doğruluk merger'daki UNIQUE kısıtına dayanır.
// Süreç yeniden başlarsa önbellek boşalır ve son pencere bir kez yeniden yayınlanır.
type Emitter struct {
	Source string
	Sink   Sink
	TTL    time.Duration // bu süredir görülmeyen kayıtlar unutulur (varsayılan 24 saat)
	Now    func() time.Time

	mu      sync.Mutex
	seen    map[string]seenEntry // source_event_id -> özet
	seenBad map[string]time.Time // ham satır özeti -> son görülme
	swept   time.Time
}

type seenEntry struct {
	digest [32]byte
	at     time.Time
}

func (e *Emitter) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func digest(r normalize.Report) [32]byte {
	d := 0.0
	if r.DepthKm != nil {
		d = *r.DepthKm
	}
	s := fmt.Sprintf("%s|%.4f|%.4f|%.2f|%.2f|%s|%s",
		r.OriginTime.UTC().Format(time.RFC3339Nano), r.Lat, r.Lon, d, r.Magnitude, r.MagType, r.PlaceName)
	return sha256.Sum256([]byte(s))
}

// EmitStats, tek bir Emit çağrısının özetidir.
type EmitStats struct {
	Published, Skipped, DLQ int
}

// Emit, sonucu yayınlar. Yayınlanamayan rapor önbelleğe işlenmez; bir sonraki
// turda yeniden denenir. Hata, en az bir yayının başarısız olduğunu bildirir.
func (e *Emitter) Emit(ctx context.Context, res Result) (EmitStats, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen == nil {
		e.seen = map[string]seenEntry{}
		e.seenBad = map[string]time.Time{}
	}
	now := e.now()
	e.sweep(now)

	var st EmitStats
	var errs []error
	for _, r := range res.Reports {
		dg := digest(r)
		prev, known := e.seen[r.SourceEventID]
		if known && prev.digest == dg {
			e.seen[r.SourceEventID] = seenEntry{dg, now}
			st.Skipped++
			continue
		}
		if err := e.Sink.Publish(ctx, r); err != nil {
			errs = append(errs, err)
			continue
		}
		e.seen[r.SourceEventID] = seenEntry{dg, now}
		st.Published++
		metrics.IngestReports.WithLabelValues(e.Source).Inc()
		if !known {
			// Gecikme yalnızca ilk görülmede ölçülür; revizyonlar ölçümü bozmasın.
			metrics.IngestLag.WithLabelValues(e.Source).Observe(r.ReceivedAt.Sub(r.OriginTime).Seconds())
		}
	}
	for _, b := range res.Bad {
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(b.Raw)))
		if _, ok := e.seenBad[key]; ok {
			e.seenBad[key] = now
			continue
		}
		if err := e.Sink.DLQ(ctx, e.Source, b); err != nil {
			errs = append(errs, err)
			continue
		}
		e.seenBad[key] = now
		st.DLQ++
		metrics.DLQMessages.WithLabelValues(e.Source).Inc()
		slog.Warn("bozuk kayıt DLQ'ya gönderildi", "source", e.Source, "reason", b.Reason)
	}
	return st, errors.Join(errs...)
}

func (e *Emitter) sweep(now time.Time) {
	if now.Sub(e.swept) < 10*time.Minute {
		return
	}
	e.swept = now
	ttl := e.TTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	for k, v := range e.seen {
		if now.Sub(v.at) > ttl {
			delete(e.seen, k)
		}
	}
	for k, at := range e.seenBad {
		if now.Sub(at) > ttl {
			delete(e.seenBad, k)
		}
	}
}
