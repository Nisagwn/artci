package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/stream"
)

// ConsumeUpdates, events.updated akışını okuyup hub'a aktarır; ctx bitince döner.
//
// Her api örneği kendi consumer group'unu kullanır ("api-<ad>"): aynı grubu
// paylaşsalar mesajlar örnekler arasında bölünür, oysa her güncelleme her
// örneğin istemcilerine ulaşmalı (yayın).
func ConsumeUpdates(ctx context.Context, rdb *redis.Client, hub *Hub, name string) error {
	group := "api-" + name
	// Grup "$" ile açılır: harita yalnızca canlı akışı ister, geçmiş REST'ten
	// gelir. stream.Consumer grubu "0"dan açardı ve ilk açılışta tüm akış
	// geçmişi tek seferde istemcilere yağardı. Grup zaten varsa (BUSYGROUP)
	// kaldığı yerden devam eder.
	err := rdb.XGroupCreateMkStream(ctx, stream.EventsUpdated, group, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	// Kapanışta grubu sil: grup bu örneğe özel; ad (hostname) bir sonraki
	// kurulumda değişebileceği için sahipsiz gruplar birikmesin.
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rdb.XGroupDestroy(dctx, stream.EventsUpdated, group).Err(); err != nil {
			slog.Warn("ws consumer group silinemedi", "group", group, "err", err)
		}
	}()

	c := &stream.Consumer{RDB: rdb, Stream: stream.EventsUpdated, Group: group, Name: name}
	return c.Run(ctx, func(_ context.Context, m stream.Message) error {
		var u normalize.EventUpdate
		if err := m.Decode(&u); err != nil || u.Event.ID == "" {
			// Bozuk mesajı yeniden denemek işe yaramaz; harita için atlamak yeterli.
			slog.Warn("events.updated çözülemedi", "id", m.ID, "err", err)
			return nil
		}
		hub.Add(u)
		return nil
	})
}
