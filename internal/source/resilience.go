package source

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Backoff, exponential backoff + full jitter üretir (Plan §9).
// Full jitter: bekleme [0, min(Max, Base·2^n)] aralığından rastgele seçilir;
// böylece aynı anda düşen istemciler kaynağa aynı anda yüklenmez.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
	Rand func() float64 // [0,1); testte sabitlenir
}

// Next, n. ardışık başarısızlıktan (n>=1) sonraki bekleme süresidir.
func (b Backoff) Next(n int) time.Duration {
	base, maxD := b.Base, b.Max
	if base <= 0 {
		base = time.Second
	}
	if maxD <= 0 {
		maxD = 2 * time.Minute
	}
	ceil := base
	for i := 1; i < n && ceil < maxD; i++ {
		ceil *= 2
	}
	if ceil > maxD {
		ceil = maxD
	}
	r := rand.Float64
	if b.Rand != nil {
		r = b.Rand
	}
	// Alt sınır base/2: çok kısa beklemelerle kaynağı boşuna yormayalım.
	d := time.Duration(r() * float64(ceil))
	if d < base/2 {
		d = base / 2
	}
	return d
}

// Jitter, d'yi ±frac oranında rastgele kaydırır (düzenli sorgu aralığı için).
func Jitter(d time.Duration, frac float64, r func() float64) time.Duration {
	if r == nil {
		r = rand.Float64
	}
	return d + time.Duration((r()*2-1)*frac*float64(d))
}

// BreakerState, circuit breaker durumudur.
type BreakerState int

const (
	BreakerClosed   BreakerState = iota // normal
	BreakerOpen                         // kaynak çökük kabul ediliyor; istek atılmıyor
	BreakerHalfOpen                     // bekleme bitti; tek deneme isteğine izin var
)

func (s BreakerState) String() string {
	return [...]string{"closed", "open", "half-open"}[s]
}

// Breaker, kaynak başına basit bir circuit breaker'dır.
// Threshold ardışık hatadan sonra açılır, Cooldown boyunca isteğe izin vermez,
// sonra yarı açık duruma geçip tek bir deneme yapar: başarılıysa kapanır,
// başarısızsa yeniden Cooldown kadar açık kalır.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration
	Now       func() time.Time

	mu       sync.Mutex
	state    BreakerState
	failures int
	openedAt time.Time
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Allow, şu an istek atılıp atılamayacağını söyler. Atılamıyorsa ne kadar
// sonra tekrar sorulması gerektiğini de döner.
func (b *Breaker) Allow() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != BreakerOpen {
		return true, 0
	}
	if wait := b.Cooldown - b.now().Sub(b.openedAt); wait > 0 {
		return false, wait
	}
	b.state = BreakerHalfOpen
	return true, 0
}

// Success, başarılı isteği kaydeder.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.failures = BreakerClosed, 0
}

// Failure, başarısız isteği kaydeder. Breaker bu çağrıyla açıldıysa true döner.
func (b *Breaker) Failure() (opened bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	th := b.Threshold
	if th <= 0 {
		th = 5
	}
	if b.state == BreakerHalfOpen || (b.state == BreakerClosed && b.failures >= th) {
		b.state, b.openedAt = BreakerOpen, b.now()
		return true
	}
	return false
}

// State, mevcut durumu döner.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// sleep, ctx iptal edilirse erken döner; iptal edildiyse false.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
