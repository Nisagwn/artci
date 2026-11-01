package source

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakeFiller struct {
	mu     sync.Mutex
	since  []time.Time
	result Result
}

func (f *fakeFiller) Since(_ context.Context, since time.Time) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.since = append(f.since, since)
	return f.result, nil
}

func (f *fakeFiller) calls() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.since...)
}

// Bağlantı kopar, yeniden bağlanılır: iki mesaj da gelir, boşluk her
// bağlantıda doldurulur ve iki doldurmadan gelen aynı olay bir kez yayınlanır.
func TestEMSCStream_ReconnectAndGapFill(t *testing.T) {
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(readFixture(t, "emsc_ws.jsonl")))
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}

	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n := conns.Add(1)
		ctx := r.Context()
		switch n {
		case 1:
			c.Write(ctx, websocket.MessageText, lines[0])
			c.Write(ctx, websocket.MessageText, []byte(`{"action":"create","data":{"properties":{"time":"bozuk"}}}`))
			c.Close(websocket.StatusInternalError, "sunucu düştü")
		default:
			c.Write(ctx, websocket.MessageText, lines[1])
			// Bağlantıyı açık tut; istemci kapatana kadar oku.
			c.CloseRead(ctx)
			<-ctx.Done()
		}
	}))
	defer srv.Close()

	fdsn, err := ParseEMSCFDSN(readFixture(t, "emsc_fdsn.json"), recv)
	if err != nil {
		t.Fatal(err)
	}
	filler := &fakeFiller{result: fdsn}
	sink := &fakeSink{}
	s := &EMSCStream{
		URL:       "ws" + strings.TrimPrefix(srv.URL, "http"),
		Filler:    filler,
		Emitter:   &Emitter{Source: "emsc", Sink: sink},
		Backoff:   Backoff{Base: 10 * time.Millisecond, Max: 50 * time.Millisecond},
		GapMargin: 15 * time.Minute,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() { s.Run(ctx); close(done) }()

	deadline := time.After(5 * time.Second)
	for {
		pub, _ := sink.count()
		if pub >= 3 && len(filler.calls()) >= 2 {
			break
		}
		select {
		case <-deadline:
			pub, dlq := sink.count()
			t.Fatalf("zaman aşımı: yayın=%d dlq=%d doldurma=%d bağlantı=%d", pub, dlq, len(filler.calls()), conns.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run kapanmadı")
	}

	// 2 akış mesajı (create + update) + FDSN'den 1 olay; ikinci doldurmada tekrar yok.
	// DLQ: akıştaki bozuk mesaj + FDSN'deki bozuk kayıt (ikinci doldurmada tekrar yazılmaz).
	pub, dlq := sink.count()
	if pub != 3 || dlq != 2 {
		t.Errorf("yayın=%d (3 beklenir) dlq=%d (2 beklenir)", pub, dlq)
	}
	calls := filler.calls()
	if d := start.Sub(calls[0]); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("ilk doldurma ~1 saat geriden başlamalı: %v", d)
	}
	if d := start.Sub(calls[1]); d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("ikinci doldurma kopma anından 15 dk geriden başlamalı: %v", d)
	}
}
