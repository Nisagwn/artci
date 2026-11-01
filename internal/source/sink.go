package source

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/stream"
)

// StreamSink, raporları reports.raw, bozuk kayıtları reports.dlq akışına yazar.
type StreamSink struct {
	RDB *redis.Client
}

func (s StreamSink) Publish(ctx context.Context, r normalize.Report) error {
	_, err := stream.Publish(ctx, s.RDB, stream.ReportsRaw, r)
	return err
}

func (s StreamSink) DLQ(ctx context.Context, source string, b BadRecord) error {
	return stream.PublishDLQ(ctx, s.RDB, source, b.Reason, b.Raw)
}
