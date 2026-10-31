package merger

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
	"github.com/nisaguven/artci/internal/stream"
)

// PGStore, *store.Store'u merger.Store arayüzüne uyarlar.
type PGStore struct{ S *store.Store }

func (p PGStore) InTx(ctx context.Context, fn func(Tx) error) error {
	return p.S.InReportTx(ctx, func(tx *store.ReportTx) error { return fn(tx) })
}

func (p PGStore) UnpublishedEvents(ctx context.Context) ([]normalize.EventUpdate, error) {
	return p.S.UnpublishedEvents(ctx)
}

func (p PGStore) MarkPublished(ctx context.Context, id string, version int, mag float64) error {
	return p.S.MarkPublished(ctx, id, version, mag)
}

// StreamPublisher, olayları Redis'teki events.updated akışına yazar.
type StreamPublisher struct{ RDB *redis.Client }

func (p StreamPublisher) Publish(ctx context.Context, u normalize.EventUpdate) error {
	_, err := stream.Publish(ctx, p.RDB, stream.EventsUpdated, u)
	return err
}

// IsPermanent, yeniden denemekle düzelmeyecek hataları ayırır: veri hatası
// (SQLSTATE sınıf 22, ör. sayısal taşma) ve kısıt ihlali (23). Bunlar DLQ'ya
// gider; geri kalan her şey (bağlantı kopması, zaman aşımı, kilitlenme) geçici
// sayılır ve mesaj ACK'lenmeden bırakılır.
func IsPermanent(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && len(pg.Code) >= 2 {
		return pg.Code[:2] == "22" || pg.Code[:2] == "23"
	}
	return false
}
