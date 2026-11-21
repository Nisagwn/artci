package stream

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func redisConsumer(t *testing.T) (context.Context, Consumer) {
	t.Helper()
	addr := os.Getenv("ARTCI_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("ARTCI_TEST_REDIS_ADDR tanımlı değil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	rdb := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	t.Cleanup(func() { rdb.Close() })
	c := Consumer{RDB: rdb, Stream: "test:stream:" + uuid.NewString(), Group: "test", Name: "worker", Batch: 1, Block: 20 * time.Millisecond, ClaimIdle: time.Hour}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := rdb.Del(cleanup, c.Stream).Err(); err != nil {
			t.Error(err)
		}
	})
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, c
}

func publishTestMessages(t *testing.T, ctx context.Context, c Consumer, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id, err := Publish(ctx, c.RDB, c.Stream, i)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func readWithoutAck(t *testing.T, ctx context.Context, c Consumer, name string, count int) {
	t.Helper()
	res, err := c.RDB.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: c.Group, Consumer: name, Streams: []string{c.Stream, ">"}, Count: int64(count), Block: -1,
	}).Result()
	if err != nil || len(res) != 1 || len(res[0].Messages) != count {
		t.Fatalf("XREADGROUP: %+v, %v", res, err)
	}
}

func expectPending(t *testing.T, ctx context.Context, c Consumer, count int64) {
	t.Helper()
	pending, err := c.RDB.XPending(ctx, c.Stream, c.Group).Result()
	if err != nil || pending.Count != count {
		t.Fatalf("XPENDING: %+v, %v; beklenen sayı %d", pending, err, count)
	}
}

func TestRedisIntegrationRestartsAcrossOwnPendingPages(t *testing.T) {
	ctx, c := redisConsumer(t)
	ids := publishTestMessages(t, ctx, c, 4)
	readWithoutAck(t, ctx, c, c.Name, 3)
	// Budanmış kayıttan kalan PEL girdisi işleyiciye boş rapor olarak gitmemeli.
	if err := c.RDB.XDel(ctx, c.Stream, ids[1]).Err(); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var handled []string
	err := c.Run(runCtx, func(_ context.Context, m Message) error {
		handled = append(handled, m.ID)
		if m.ID == ids[0] {
			return errors.New("geçici işleyici hatası")
		}
		if m.ID == ids[3] {
			stop()
		}
		return nil
	})
	if want := []string{ids[0], ids[2], ids[3]}; err != nil || !slices.Equal(handled, want) {
		t.Fatalf("işlenenler: %v, hata: %v; beklenen %v", handled, err, want)
	}
	expectPending(t, ctx, c, 1)
}

func TestRedisIntegrationClaimsPastEmptyPage(t *testing.T) {
	ctx, c := redisConsumer(t)
	ids := publishTestMessages(t, ctx, c, 16)
	readWithoutAck(t, ctx, c, "stopped-worker", 15)
	stale := ids[12:15]
	args := []any{"XCLAIM", c.Stream, c.Group, "stopped-worker", 0, stale[0], stale[1], stale[2], "IDLE", (2 * time.Hour).Milliseconds()}
	if err := c.RDB.Do(ctx, args...).Err(); err != nil {
		t.Fatal(err)
	}
	first, next, err := c.RDB.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: c.Stream, Group: c.Group, Consumer: c.Name, MinIdle: c.ClaimIdle, Start: "0-0", Count: c.Batch,
	}).Result()
	if err != nil || len(first) != 0 || next == "0-0" {
		t.Fatalf("boş ilk sayfa bekleniyordu: %v, sonraki: %s, hata: %v", first, next, err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var handled []string
	err = c.Run(runCtx, func(_ context.Context, m Message) error {
		handled = append(handled, m.ID)
		if m.ID == stale[2] {
			stop()
		}
		return nil
	})
	if want := append([]string{ids[15]}, stale...); err != nil || !slices.Equal(handled, want) {
		t.Fatalf("işlenenler: %v, hata: %v; beklenen %v", handled, err, want)
	}
	expectPending(t, ctx, c, 12)
}

type failFirstAck struct{ failed bool }

func (*failFirstAck) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*failFirstAck) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f *failFirstAck) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "xack" && !f.failed {
			f.failed = true
			return errors.New("test ACK hatası")
		}
		return next(ctx, cmd)
	}
}

func TestRedisIntegrationAckErrorLeavesMessagePending(t *testing.T) {
	ctx, c := redisConsumer(t)
	id := publishTestMessages(t, ctx, c, 1)[0]
	c.RDB.AddHook(&failFirstAck{})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, pending := range []int64{1, 0} {
		runCtx, stop := context.WithCancel(ctx)
		var handled string
		err := c.Run(runCtx, func(_ context.Context, m Message) error {
			handled = m.ID
			stop()
			return nil
		})
		stop()
		if err != nil || handled != id {
			t.Fatalf("işlenen: %q, hata: %v; beklenen %q", handled, err, id)
		}
		expectPending(t, ctx, c, pending)
	}
	for _, value := range []string{c.Stream, "group=" + c.Group, "msg_id=" + id, "test ACK hatası"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("ACK kaydında %q yok: %s", value, &logs)
		}
	}
}
