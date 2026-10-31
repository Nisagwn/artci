package source

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

func TestBackoff(t *testing.T) {
	max1 := func() float64 { return 0.999999 }
	b := Backoff{Base: time.Second, Max: 10 * time.Second, Rand: max1}
	want := []time.Duration{1, 2, 4, 8, 10, 10}
	for i, w := range want {
		got := b.Next(i + 1).Round(time.Second)
		if got != w*time.Second {
			t.Errorf("Next(%d)=%v, beklenen %v", i+1, got, w*time.Second)
		}
	}
	b.Rand = func() float64 { return 0 }
	if got := b.Next(5); got != 500*time.Millisecond {
		t.Errorf("alt sınır base/2 olmalı, %v", got)
	}
}

func TestJitter(t *testing.T) {
	if got := Jitter(10*time.Second, 0.1, func() float64 { return 0 }); got != 9*time.Second {
		t.Errorf("%v", got)
	}
	if got := Jitter(10*time.Second, 0.1, func() float64 { return 1 }); got != 11*time.Second {
		t.Errorf("%v", got)
	}
}

func TestBreaker(t *testing.T) {
	now := time.Unix(0, 0)
	b := &Breaker{Threshold: 3, Cooldown: time.Minute, Now: func() time.Time { return now }}

	for i := 0; i < 2; i++ {
		if b.Failure() {
			t.Fatal("eşikten önce açılmamalı")
		}
	}
	if !b.Failure() || b.State() != BreakerOpen {
		t.Fatal("3. hatada açılmalı")
	}
	if ok, wait := b.Allow(); ok || wait != time.Minute {
		t.Fatalf("açıkken izin yok: ok=%v wait=%v", ok, wait)
	}
	now = now.Add(time.Minute)
	if ok, _ := b.Allow(); !ok || b.State() != BreakerHalfOpen {
		t.Fatal("bekleme sonrası yarı açık olmalı")
	}
	if !b.Failure() || b.State() != BreakerOpen {
		t.Fatal("yarı açıkken tek hata yeniden açmalı")
	}
	now = now.Add(time.Minute)
	b.Allow()
	b.Success()
	if b.State() != BreakerClosed {
		t.Fatal("başarılı deneme kapatmalı")
	}
	if b.Failure() {
		t.Fatal("kapandıktan sonra sayaç sıfırlanmalı")
	}
}

// --- Emitter ---

type fakeSink struct {
	mu        sync.Mutex
	published []normalize.Report
	dlq       []BadRecord
	failPub   bool
}

func (s *fakeSink) Publish(_ context.Context, r normalize.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPub {
		return errors.New("redis yok")
	}
	s.published = append(s.published, r)
	return nil
}

func (s *fakeSink) DLQ(_ context.Context, _ string, b BadRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dlq = append(s.dlq, b)
	return nil
}

func (s *fakeSink) count() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.published), len(s.dlq)
}

func rpt(id string, mag float64) normalize.Report {
	return normalize.Report{Source: "afad", SourceEventID: id, OriginTime: recv.Add(-time.Minute),
		Lat: 39, Lon: 28, Magnitude: mag, ReceivedAt: recv}
}

func TestEmitter_Dedup(t *testing.T) {
	sink := &fakeSink{}
	e := &Emitter{Source: "afad", Sink: sink}
	ctx := context.Background()
	res := Result{Reports: []normalize.Report{rpt("1", 4.1), rpt("2", 3.0)}, Bad: []BadRecord{{Raw: "x", Reason: "bozuk"}}}

	st, err := e.Emit(ctx, res)
	if err != nil || st.Published != 2 || st.DLQ != 1 {
		t.Fatalf("ilk tur: %+v %v", st, err)
	}
	st, _ = e.Emit(ctx, res)
	if st.Published != 0 || st.Skipped != 2 || st.DLQ != 0 {
		t.Fatalf("aynı sonuç tekrar yayınlanmamalı: %+v", st)
	}
	// Revizyon: aynı kimlik, farklı büyüklük -> yeniden yayınlanır.
	st, _ = e.Emit(ctx, Result{Reports: []normalize.Report{rpt("1", 4.3)}})
	if st.Published != 1 {
		t.Fatalf("revizyon yayınlanmalı: %+v", st)
	}
}

func TestEmitter_PublishFailureRetried(t *testing.T) {
	sink := &fakeSink{failPub: true}
	e := &Emitter{Source: "afad", Sink: sink}
	res := Result{Reports: []normalize.Report{rpt("1", 4.1)}}
	if _, err := e.Emit(context.Background(), res); err == nil {
		t.Fatal("yayın hatası bildirilmeli")
	}
	sink.failPub = false
	st, err := e.Emit(context.Background(), res)
	if err != nil || st.Published != 1 {
		t.Fatalf("başarısız yayın önbelleğe girmemeli, yeniden denenmeli: %+v %v", st, err)
	}
}

func TestEmitter_TTL(t *testing.T) {
	now := recv
	sink := &fakeSink{}
	e := &Emitter{Source: "afad", Sink: sink, TTL: time.Hour, Now: func() time.Time { return now }}
	res := Result{Reports: []normalize.Report{rpt("1", 4.1)}}
	e.Emit(context.Background(), res)
	now = now.Add(2 * time.Hour)
	if st, _ := e.Emit(context.Background(), res); st.Published != 1 {
		t.Fatalf("TTL sonrası unutulmalı: %+v", st)
	}
}

// --- Poller ---

type fakeFetcher struct {
	err   error
	res   Result
	calls int
}

func (f *fakeFetcher) Name() string { return "afad" }
func (f *fakeFetcher) Fetch(context.Context) (Result, error) {
	f.calls++
	return f.res, f.err
}

func TestPoller_BackoffAndBreaker(t *testing.T) {
	f := &fakeFetcher{err: errors.New("503")}
	sink := &fakeSink{}
	p := &Poller{
		Fetcher: f, Emitter: &Emitter{Source: "afad", Sink: sink}, Interval: 30 * time.Second,
		Backoff: Backoff{Base: time.Second, Max: time.Minute, Rand: func() float64 { return 0.999 }},
		Breaker: &Breaker{Threshold: 3, Cooldown: time.Minute},
	}
	ctx := context.Background()
	failures := 0
	var waits []time.Duration
	for i := 0; i < 3; i++ {
		waits = append(waits, p.tick(ctx, "afad", &failures).Round(time.Second))
	}
	if waits[0] >= waits[1] || waits[1] >= waits[2] {
		t.Errorf("bekleme artmalı: %v", waits)
	}
	if p.Breaker.State() != BreakerOpen {
		t.Fatal("3 hatadan sonra breaker açık olmalı")
	}

	// Kaynak geri geldi.
	f.err, f.res = nil, Result{Reports: []normalize.Report{rpt("1", 4.1)}}
	p.Breaker = &Breaker{Threshold: 3, Cooldown: time.Minute}
	w := p.tick(ctx, "afad", &failures)
	if failures != 0 || w < 27*time.Second || w > 33*time.Second {
		t.Errorf("başarıda sayaç sıfırlanmalı ve aralık ~30 sn olmalı: failures=%d wait=%v", failures, w)
	}
	if n, _ := sink.count(); n != 1 {
		t.Errorf("rapor yayınlanmalı, %d", n)
	}
}

func TestPoller_RunStopsOnCancel(t *testing.T) {
	f := &fakeFetcher{}
	p := &Poller{Fetcher: f, Emitter: &Emitter{Source: "afad", Sink: &fakeSink{}}, Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run, iptalden sonra dönmeli")
	}
	if f.calls != 1 {
		t.Errorf("ilk tur hemen çalışmalı, calls=%d", f.calls)
	}
}
