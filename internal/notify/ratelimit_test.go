package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newTestLimiter(g, c float64) (*Limiter, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := NewLimiter(g, c)
	l.now = clk.now
	return l, clk
}

func TestLimiterPerChat(t *testing.T) {
	l, clk := newTestLimiter(25, 1)
	if d := l.Reserve(1); d != 0 {
		t.Fatalf("ilk mesaj beklememeli: %v", d)
	}
	if d := l.Reserve(1); d != time.Second {
		t.Fatalf("aynı sohbete ikinci mesaj 1 sn beklemeli: %v", d)
	}
	if d := l.Reserve(1); d != 2*time.Second {
		t.Fatalf("üçüncü mesaj 2 sn beklemeli (rezervasyon birikir): %v", d)
	}
	if d := l.Reserve(2); d != 0 {
		t.Fatalf("başka sohbet etkilenmemeli: %v", d)
	}
	clk.add(3 * time.Second)
	if d := l.Reserve(1); d != 0 {
		t.Fatalf("borç ödendikten sonra beklememeli: %v", d)
	}
}

func TestLimiterGlobal(t *testing.T) {
	l, clk := newTestLimiter(25, 1)
	// 25 farklı sohbet: patlama kapasitesi içinde.
	for i := range int64(25) {
		if d := l.Reserve(i); d != 0 {
			t.Fatalf("%d. mesaj beklememeli: %v", i, d)
		}
	}
	// 26. mesaj 1/25 sn beklemeli.
	if d := l.Reserve(100); d != 40*time.Millisecond {
		t.Fatalf("genel sınır: %v", d)
	}
	clk.add(time.Second)
	if d := l.Reserve(101); d != 0 {
		t.Fatalf("1 sn sonra kova dolmalı: %v", d)
	}
}

func TestLimiterPrune(t *testing.T) {
	l, clk := newTestLimiter(25, 1)
	l.Reserve(1)
	clk.add(2 * time.Minute)
	l.Reserve(2)
	if _, ok := l.chats[1]; ok || len(l.chats) != 1 {
		t.Fatalf("sessiz sohbet kovası silinmeliydi: %v", l.chats)
	}
}

func TestQueueSendsAndReportsErrors(t *testing.T) {
	var mu sync.Mutex
	var sent []int64
	boom := errors.New("boom")
	q := NewQueue(func(_ context.Context, chatID int64, _ string) error {
		if chatID == 13 {
			return boom
		}
		mu.Lock()
		sent = append(sent, chatID)
		mu.Unlock()
		return nil
	}, NewLimiter(1000, 1000), 3, 10)

	ctx := context.Background()
	results := []<-chan error{q.Submit(ctx, 1, "a"), q.Submit(ctx, 13, "b"), q.Submit(ctx, 2, "c")}
	if err := <-results[0]; err != nil {
		t.Fatal(err)
	}
	if err := <-results[1]; !errors.Is(err, boom) {
		t.Fatalf("hata iletilmeli: %v", err)
	}
	if err := <-results[2]; err != nil {
		t.Fatal(err)
	}
	q.Close()
	if len(sent) != 2 {
		t.Fatalf("sent = %v", sent)
	}
}

func TestQueueRespectsChatLimit(t *testing.T) {
	// Gerçek saatle: aynı sohbete 3 mesaj, 10 msg/sn sohbet sınırıyla >= ~200 ms sürmeli.
	q := NewQueue(func(context.Context, int64, string) error { return nil }, NewLimiter(1000, 10), 4, 10)
	defer q.Close()
	start := time.Now()
	ctx := context.Background()
	a, b, c := q.Submit(ctx, 7, "1"), q.Submit(ctx, 7, "2"), q.Submit(ctx, 7, "3")
	<-a
	<-b
	<-c
	if el := time.Since(start); el < 180*time.Millisecond {
		t.Fatalf("hız sınırı uygulanmadı: %v", el)
	}
}

func TestQueueSubmitCanceled(t *testing.T) {
	block := make(chan struct{})
	q := NewQueue(func(context.Context, int64, string) error { <-block; return nil }, NewLimiter(1000, 1000), 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	first := q.Submit(context.Background(), 1, "x") // işçiyi meşgul eder
	cancel()
	if err := <-q.Submit(ctx, 2, "y"); !errors.Is(err, context.Canceled) {
		t.Fatalf("iptal bekleniyordu: %v", err)
	}
	close(block)
	<-first
	q.Close()
}
