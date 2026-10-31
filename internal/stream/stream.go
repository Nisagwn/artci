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
	lastClaim := time.Now()
	for ctx.Err() == nil {
		if time.Since(lastClaim) > c.ClaimIdle {
			c.claimStale(ctx, work, h)
			lastClaim = time.Now()
		}
		res, err := c.RDB.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: c.Group, Consumer: c.Name, Streams: []string{c.Stream, ">"},
			Count: c.Batch, Block: c.Block,
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
	res, err := c.RDB.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: c.Group, Consumer: c.Name, Streams: []string{c.Stream, "0"}, Count: 1000,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	for _, s := range res {
		c.handleAll(work, s.Messages, h)
	}
	return nil
}

func (c *Consumer) claimStale(ctx, work context.Context, h Handler) {
	msgs, _, err := c.RDB.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: c.Stream, Group: c.Group, Consumer: c.Name,
		MinIdle: c.ClaimIdle, Start: "0", Count: c.Batch,
	}).Result()
	if err == nil {
		c.handleAll(work, msgs, h)
	}
}

func (c *Consumer) handleAll(ctx context.Context, msgs []redis.XMessage, h Handler) {
	for _, xm := range msgs {
		data, _ := xm.Values["data"].(string)
		if err := h(ctx, Message{ID: xm.ID, Data: []byte(data)}); err != nil {
			continue // ACK yok -> sonra yeniden denenecek
		}
		c.RDB.XAck(ctx, c.Stream, c.Group, xm.ID)
	}
}
