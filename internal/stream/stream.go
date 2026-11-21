// Package stream, Redis Streams üzerinde at-least-once yayın/tüketim yardımcılarıdır.
//
// Model: her mesajın "data" alanında JSON gövde bulunur. Tüketiciler bir consumer
// group içinde çalışır; işlenen mesaj ACK'lenir. Çöken tüketicinin onaylanmamış
// mesajları XAUTOCLAIM ile başka (veya yeniden başlayan) tüketiciye devredilir.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Akış adları (Plan §6).
const (
	ReportsRaw    = "reports.raw"
	ReportsDLQ    = "reports.dlq"
	EventsUpdated = "events.updated"
)

// MaxLen: akışların yaklaşık üst sınırı (bellek tavanı).
const MaxLen = 100_000

const pendingStart = "0-0"

// Publish, v'yi JSON olarak akışa ekler.
func Publish(ctx context.Context, rdb *redis.Client, streamName string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	return rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: streamName,
		MaxLen: MaxLen,
		Approx: true,
		Values: map[string]any{"data": b},
	}).Result()
}

// DLQEntry, ayrıştırılamayan veya doğrulanamayan kayıttır.
type DLQEntry struct {
	Source string    `json:"source"`
	Reason string    `json:"reason"`
	Raw    string    `json:"raw"`
	At     time.Time `json:"at"`
}

// PublishDLQ, bozuk bir kaydı DLQ akışına yazar.
func PublishDLQ(ctx context.Context, rdb *redis.Client, source, reason, raw string) error {
	_, err := Publish(ctx, rdb, ReportsDLQ, DLQEntry{Source: source, Reason: reason, Raw: raw, At: time.Now().UTC()})
	return err
}

// Message, tüketilen tek bir mesajdır.
type Message struct {
	ID   string
	Data []byte
}

// Decode, mesaj gövdesini v'ye çözer.
func (m Message) Decode(v any) error { return json.Unmarshal(m.Data, v) }

// Handler bir mesajı işler. nil dönerse mesaj ACK'lenir; hata dönerse ACK'lenmez
// ve ClaimIdle süresi sonra yeniden denenir. Kalıcı olarak işlenemeyen mesajlar
// için handler kendisi DLQ'ya yazıp nil dönmelidir (aksi halde sonsuz döngü).
type Handler func(ctx context.Context, m Message) error

// Consumer, bir consumer group üyesidir.
type Consumer struct {
	RDB       *redis.Client
	Stream    string
	Group     string
	Name      string        // tüketici adı (ör. hostname)
	Batch     int64         // tek okumada en fazla mesaj
	Block     time.Duration // yeni mesaj bekleme süresi
	ClaimIdle time.Duration // bu süreden uzun bekleyen onaysız mesajlar devralınır
}

// EnsureGroup, grup yoksa oluşturur (akış yoksa da oluşturur).
func (c *Consumer) EnsureGroup(ctx context.Context) error {
	err := c.RDB.XGroupCreateMkStream(ctx, c.Stream, c.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

func (c *Consumer) defaults() {
	if c.Batch == 0 {
		c.Batch = 64
	}
	if c.Block == 0 {
		c.Block = 2 * time.Second
	}
	if c.ClaimIdle == 0 {
		c.ClaimIdle = 30 * time.Second
	}
}

// Run, ctx iptal edilene kadar mesaj işler. Graceful shutdown: ctx iptal edildiğinde
// yeni okuma yapılmaz; elde olan mesajlar işlenip ACK'lenir, sonra döner.
// Mesajlar sırayla (tek goroutine) işlenir; merger için bu bilinçli bir tercihtir.
func (c *Consumer) Run(ctx context.Context, h Handler) error {
	c.defaults()
	if err := c.EnsureGroup(ctx); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	// İşleme için ayrı bağlam: kapanışta yarıdaki mesaj iptal edilmesin.
	work := context.WithoutCancel(ctx)

	// Önce bu tüketicinin kendi bekleyen mesajları (önceki çöküşten kalanlar).
	if err := c.drainOwnPending(ctx, work, h); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	claimCursor := pendingStart
	var nextClaim time.Time
	for ctx.Err() == nil {
		block := c.Block
		if !time.Now().Before(nextClaim) {
			next, err := c.claimStale(ctx, work, claimCursor, h)
			if ctx.Err() != nil {
				return nil
			}
			switch {
			case err != nil:
				slog.Warn("bekleyen Redis mesajları devralınamadı", "stream", c.Stream, "group", c.Group, "err", err)
				nextClaim = time.Now().Add(c.ClaimIdle)
			case next == pendingStart:
				claimCursor = pendingStart
				nextClaim = time.Now().Add(c.ClaimIdle)
			default:
				claimCursor = next
				// Sonraki devralma sayfasından önce yeni mesajlara da sıra ver.
				// Negatif Block, Redis komutuna BLOCK seçeneğini eklemez.
				block = -1
			}
		}
		res, err := c.RDB.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: c.Group, Consumer: c.Name, Streams: []string{c.Stream, ">"},
			Count: c.Batch, Block: block,
		}).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			time.Sleep(time.Second)
			continue
		}
		for _, s := range res {
			c.handleAll(work, s.Messages, h)
		}
	}
	return nil
}

func (c *Consumer) drainOwnPending(ctx, work context.Context, h Handler) error {
	cursor := pendingStart
	for ctx.Err() == nil {
		res, err := c.RDB.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: c.Group, Consumer: c.Name, Streams: []string{c.Stream, cursor}, Count: c.Batch, Block: -1,
		}).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read own pending: %w", err)
		}
		if len(res) == 0 || len(res[0].Messages) == 0 {
			return nil
		}
		messages := res[0].Messages
		c.handleAll(work, messages, h)
		// Hata alan bir mesaj bu taramayı tutmasın; daha sonra devralınır.
		cursor = messages[len(messages)-1].ID
	}
	return nil
}

func (c *Consumer) claimStale(ctx, work context.Context, cursor string, h Handler) (string, error) {
	msgs, next, err := c.RDB.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: c.Stream, Group: c.Group, Consumer: c.Name,
		MinIdle: c.ClaimIdle, Start: cursor, Count: c.Batch,
	}).Result()
	if err != nil {
		return cursor, err
	}
	c.handleAll(work, msgs, h)
	return next, nil
}

func (c *Consumer) handleAll(ctx context.Context, msgs []redis.XMessage, h Handler) {
	for _, xm := range msgs {
		data, _ := xm.Values["data"].(string)
		if xm.Values == nil {
			// XREADGROUP geçmişinde, gövdesi budanmış bir mesajın kimliği kalabilir.
			slog.Warn("bekleyen Redis mesajının gövdesi silinmiş", "stream", c.Stream, "group", c.Group, "msg_id", xm.ID)
		} else if err := h(ctx, Message{ID: xm.ID, Data: []byte(data)}); err != nil {
			continue // ACK yok -> sonra yeniden denenecek
		}
		if err := c.RDB.XAck(ctx, c.Stream, c.Group, xm.ID).Err(); err != nil {
			slog.Warn("Redis mesajı onaylanamadı", "stream", c.Stream, "group", c.Group, "msg_id", xm.ID, "err", err)
		}
	}
}
