package notify

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/store"
)

type memBotStore struct {
	mu   sync.Mutex
	subs []store.Subscription
	next int64
}

func (m *memBotStore) AddSubscription(_ context.Context, s store.Subscription) (store.Subscription, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.subs {
		if e.ChatID == s.ChatID && e.City == s.City {
			s.ID = e.ID
			m.subs[i] = s
			return s, true, nil
		}
	}
	m.next++
	s.ID = m.next
	m.subs = append(m.subs, s)
	return s, false, nil
}

func (m *memBotStore) ListSubscriptions(_ context.Context, chat int64) ([]store.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Subscription
	for _, s := range m.subs {
		if s.ChatID == chat {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memBotStore) DeleteSubscriptions(_ context.Context, chat int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keep []store.Subscription
	for _, s := range m.subs {
		if s.ChatID != chat {
			keep = append(keep, s)
		}
	}
	n := int64(len(m.subs) - len(keep))
	m.subs = keep
	return n, nil
}

func newTestBot() (*Bot, *memBotStore) {
	ms := &memBotStore{}
	return &Bot{Store: ms, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, ms
}

func TestBotReplyFlow(t *testing.T) {
	b, ms := newTestBot()
	ctx := context.Background()
	const chat = 42

	if got := b.Reply(ctx, chat, "/start"); !strings.Contains(got, "Bu bir eğitim projesidir; resmî bilgi için AFAD'ı takip edin.") {
		t.Fatalf("/start tam uyarıyı içermeli: %s", got)
	}
	if got := b.Reply(ctx, chat, "/aboneliklerim"); !strings.Contains(got, "Henüz aboneliğiniz yok") {
		t.Fatalf("boş liste: %s", got)
	}
	got := b.Reply(ctx, chat, "/abone Balıkesir 100 3,5")
	if !strings.Contains(got, "Abonelik eklendi") || !strings.Contains(got, "<b>Balıkesir</b> merkezinin 100 km") || !strings.Contains(got, "3,5 ve üzeri") {
		t.Fatalf("abone yanıtı: %s", got)
	}
	if got := b.Reply(ctx, chat, "/abone balikesir 50 4"); !strings.Contains(got, "Abonelik güncellendi") {
		t.Fatalf("aynı il güncellenmeli: %s", got)
	}
	if len(ms.subs) != 1 || ms.subs[0].RadiusKm != 50 || ms.subs[0].MinMag != 4 {
		t.Fatalf("subs = %+v", ms.subs)
	}
	if got := b.Reply(ctx, chat, "/aboneliklerim"); !strings.Contains(got, "• Balıkesir: 50 km, 4,0 ve üzeri") {
		t.Fatalf("liste: %s", got)
	}
	if got := b.Reply(ctx, chat, "/abone Atlantis"); !strings.Contains(got, "bulamadım") {
		t.Fatalf("bilinmeyen il: %s", got)
	}
	if got := b.Reply(ctx, chat, "/iptal"); !strings.Contains(got, "1 aboneliğiniz") {
		t.Fatalf("iptal: %s", got)
	}
	if len(ms.subs) != 0 {
		t.Fatalf("iptal sonrası kayıt kalmamalı: %+v", ms.subs)
	}
	if got := b.Reply(ctx, chat, "/iptal"); !strings.Contains(got, "Kayıtlı aboneliğiniz yok") {
		t.Fatalf("ikinci iptal: %s", got)
	}
}

func TestBotEveryReplyHasDisclaimer(t *testing.T) {
	b, _ := newTestBot()
	for _, in := range []string{"/start", "/yardim", "/abone Van", "/aboneliklerim", "/iptal", "/abone", "/xyz", "selam"} {
		if got := b.Reply(context.Background(), 1, in); !strings.Contains(got, "AFAD") {
			t.Errorf("%q yanıtında uyarı yok: %s", in, got)
		}
	}
}

func TestBotEscapesUserInput(t *testing.T) {
	b, _ := newTestBot()
	got := b.Reply(context.Background(), 1, "/abone <script>")
	if strings.Contains(got, "<script>") || !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("kullanıcı girdisi kaçışlanmalı: %s", got)
	}
}

func TestBotSubscriptionLimit(t *testing.T) {
	b, _ := newTestBot()
	ctx := context.Background()
	for _, c := range []string{"Van", "Muş", "Bitlis", "Siirt", "Batman"} {
		b.Reply(ctx, 1, "/abone "+c)
	}
	if got := b.Reply(ctx, 1, "/abone İzmir"); !strings.Contains(got, "En fazla 5") {
		t.Fatalf("sınır uygulanmalı: %s", got)
	}
	if got := b.Reply(ctx, 1, "/abone Van 200"); !strings.Contains(got, "güncellendi") {
		t.Fatalf("sınırda mevcut il güncellenebilmeli: %s", got)
	}
}

// scriptedUpdater, bir kez güncelleme verir, sonra ctx bitene kadar bekler (long polling gibi).
type scriptedUpdater struct {
	mu      sync.Mutex
	batches [][]Update
	offsets []int64
}

func (s *scriptedUpdater) GetUpdates(ctx context.Context, offset int64, _ int) ([]Update, error) {
	s.mu.Lock()
	s.offsets = append(s.offsets, offset)
	if len(s.batches) > 0 {
		b := s.batches[0]
		s.batches = s.batches[1:]
		s.mu.Unlock()
		return b, nil
	}
	s.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func msg(id, chat int64, text string) Update {
	m := &Message{Text: text}
	m.Chat.ID = chat
	return Update{UpdateID: id, Message: m}
}

func TestBotRunAdvancesOffset(t *testing.T) {
	b, ms := newTestBot()
	up := &scriptedUpdater{batches: [][]Update{
		{msg(10, 7, "/abone Van"), {UpdateID: 11}},
		{msg(12, 7, "/aboneliklerim")},
	}}
	b.Updates = up
	var mu sync.Mutex
	var replies []string
	b.Send = func(_ context.Context, chat int64, html string) error {
		mu.Lock()
		defer mu.Unlock()
		replies = append(replies, html)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(replies)
		mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("yanıtlar gelmedi")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.offsets) < 3 || up.offsets[0] != 0 || up.offsets[1] != 12 || up.offsets[2] != 13 {
		t.Fatalf("offsets = %v", up.offsets)
	}
	if len(ms.subs) != 1 || !strings.Contains(replies[1], "Van") {
		t.Fatalf("subs=%+v replies=%v", ms.subs, replies)
	}
}
