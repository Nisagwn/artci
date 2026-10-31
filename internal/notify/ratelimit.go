package notify

import (
	"context"
	"sync"
	"time"
)

// Telegram sınırları (yaklaşık): bot başına ~30 msg/sn, sohbet başına ~1 msg/sn.
// Biraz altında kalıyoruz ki 429'a hiç düşmeyelim.
const (
	DefaultGlobalRate = 25.0
	DefaultChatRate   = 1.0
)

// bucket, klasik token bucket'tır. Rezervasyon modelinde çalışır: jeton yoksa
// borçlanılır (tokens < 0) ve borcun ödeneceği süre beklenir. Böylece aynı anda
// bekleyenler sırayla, üst üste binmeden geçer.
type bucket struct {
	rate   float64 // saniyede jeton
	burst  float64
	tokens float64
	last   time.Time
}

func (b *bucket) reserve(now time.Time) time.Duration {
	if b.last.IsZero() {
		b.tokens = b.burst
	} else if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(b.burst, b.tokens+el*b.rate)
	}
	b.last = now
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// Limiter, genel ve sohbet başına iki token bucket'ı birlikte uygular.
type Limiter struct {
	mu        sync.Mutex
	global    bucket
	chatRate  float64
	chats     map[int64]*bucket
	now       func() time.Time
	lastPrune time.Time
}

// NewLimiter: globalRate msg/sn genel, chatRate msg/sn sohbet başına.
func NewLimiter(globalRate, chatRate float64) *Limiter {
	return &Limiter{
		global:   bucket{rate: globalRate, burst: globalRate},
		chatRate: chatRate,
		chats:    map[int64]*bucket{},
		now:      time.Now,
	}
}

// Reserve, chatID'ye bir mesaj için yer ayırır ve beklenmesi gereken süreyi döner.
func (l *Limiter) Reserve(chatID int64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	cb, ok := l.chats[chatID]
	if !ok {
		cb = &bucket{rate: l.chatRate, burst: 1}
		l.chats[chatID] = cb
	}
	return max(l.global.reserve(now), cb.reserve(now))
}

// prune, uzun süredir sessiz sohbetlerin kovalarını atar; harita sınırsız büyümesin.
// Dolu bir kovayı silmek davranışı değiştirmez (yeni kova da dolu başlar).
func (l *Limiter) prune(now time.Time) {
	if now.Sub(l.lastPrune) < time.Minute {
		return
	}
	l.lastPrune = now
	for id, b := range l.chats {
		if now.Sub(b.last) > time.Minute {
			delete(l.chats, id)
		}
	}
}

// Wait, sıra gelene kadar bekler.
func (l *Limiter) Wait(ctx context.Context, chatID int64) error {
	d := l.Reserve(chatID)
	if d <= 0 {
		return ctx.Err()
	}
	return sleepCtx(ctx, d)
}

// SendFunc, tek bir mesajı gönderir (Telegram veya dry-run).
type SendFunc func(ctx context.Context, chatID int64, html string) error

type sendJob struct {
	ctx    context.Context
	chatID int64
	html   string
	done   chan error
}

// Queue, hız sınırına uyan gönderim kuyruğudur. Bot yanıtları ve bildirimler
// aynı kuyruktan geçer ki Telegram sınırı ortak tutulsun. Birden çok işçi
// vardır: tek işçide, sohbet başına beklemesi olan bir mesaj diğer sohbetleri
// de bekletirdi (head-of-line blocking).
type Queue struct {
	send    SendFunc
	limiter *Limiter
	jobs    chan sendJob
	wg      sync.WaitGroup
}

// NewQueue, workers adet işçiyle bir kuyruk başlatır. Close ile durdurulur.
func NewQueue(send SendFunc, limiter *Limiter, workers, buffer int) *Queue {
	q := &Queue{send: send, limiter: limiter, jobs: make(chan sendJob, buffer)}
	for range max(1, workers) {
		q.wg.Add(1)
		go q.worker()
	}
	return q
}

func (q *Queue) worker() {
	defer q.wg.Done()
	for j := range q.jobs {
		err := q.limiter.Wait(j.ctx, j.chatID)
		if err == nil {
			err = q.send(j.ctx, j.chatID, j.html)
		}
		j.done <- err
	}
}

// Submit, mesajı kuyruğa koyar; sonuç dönen kanala tek kez yazılır.
// Kuyruk doluysa bekler (backpressure): tüketici Telegram'dan hızlı ilerlemesin.
func (q *Queue) Submit(ctx context.Context, chatID int64, html string) <-chan error {
	done := make(chan error, 1)
	select {
	case q.jobs <- sendJob{ctx: ctx, chatID: chatID, html: html, done: done}:
	case <-ctx.Done():
		done <- ctx.Err()
	}
	return done
}

// Send, Submit edip sonucu bekler.
func (q *Queue) Send(ctx context.Context, chatID int64, html string) error {
	return <-q.Submit(ctx, chatID, html)
}

// Close, yeni iş kabul etmeyi bırakır ve kuyruktaki işler bitene kadar bekler.
// Çağıran, artık Submit yapılmayacağını garanti etmelidir.
func (q *Queue) Close() {
	close(q.jobs)
	q.wg.Wait()
}
