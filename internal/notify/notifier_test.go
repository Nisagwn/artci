package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/geo"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
)

func TestDecide(t *testing.T) {
	none := store.NotifyState{}
	initial := func(m float64) store.NotifyState { return store.NotifyState{HasInitial: true, LastMag: m} }
	tests := []struct {
		name   string
		st     store.NotifyState
		mag    float64
		minMag float64
		want   string
	}{
		{"eşik altında", none, 3.4, 3.5, ""},
		{"eşikte: ilk bildirim", none, 3.5, 3.5, store.KindInitial},
		{"eşiği bir düzeltmeyle ilk kez geçti", none, 3.8, 3.5, store.KindInitial},
		{"Senaryo 2: 4,3 -> 4,1 sessiz", initial(4.3), 4.1, 3.5, ""},
		{"0,4 artış sessiz", initial(4.1), 4.5, 3.5, ""},
		{"tam 0,5 artış (kayan nokta)", initial(4.1), 4.6, 3.5, store.KindUpdate},
		{"0,5 azalış da güncelleme", initial(5.0), 4.5, 3.5, store.KindUpdate},
		{"eşiğin altına düşen düzeltme bildirilmez", initial(4.0), 3.4, 3.5, ""},
		{"ikinci güncelleme yok", store.NotifyState{HasInitial: true, HasUpdate: true, LastMag: 4.6}, 5.5, 3.5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(tt.st, tt.mag, tt.minMag); got != tt.want {
				t.Fatalf("Decide = %q, want %q", got, tt.want)
			}
		})
	}
}

// memStore, store.Store'un bildirim günlüğü semantiğini bellekte taklit eder.
type memStore struct {
	mu      sync.Mutex
	subs    []store.Subscription
	log     map[string]float64 // eventID|subID|kind -> mag
	failAff error
}

func key(ev string, sub int64, kind string) string {
	return fmt.Sprintf("%s|%d|%s", ev, sub, kind)
}

func (m *memStore) AffectedSubscriptions(_ context.Context, ev normalize.Event) ([]store.Subscription, error) {
	if m.failAff != nil {
		return nil, m.failAff
	}
	var out []store.Subscription
	for _, s := range m.subs {
		if geo.HaversineKm(s.Lat, s.Lon, ev.Lat, ev.Lon) <= float64(s.RadiusKm) && s.MinMag <= round1(ev.Magnitude) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memStore) LastNotifiedMagnitude(_ context.Context, ev string, sub int64) (store.NotifyState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st store.NotifyState
	if v, ok := m.log[key(ev, sub, store.KindInitial)]; ok {
		st.HasInitial, st.LastMag = true, v
	}
	if v, ok := m.log[key(ev, sub, store.KindUpdate)]; ok {
		st.HasUpdate, st.LastMag = true, v
	}
	return st, nil
}

func (m *memStore) RecordNotification(_ context.Context, ev string, sub int64, kind string, mag float64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(ev, sub, kind)
	if _, ok := m.log[k]; ok {
		return false, nil
	}
	m.log[k] = mag
	return true, nil
}

func (m *memStore) DeleteNotification(_ context.Context, ev string, sub int64, kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.log, key(ev, sub, kind))
	return nil
}

type sentMsg struct {
	chat int64
	html string
}

type fakeSender struct {
	mu   sync.Mutex
	sent []sentMsg
	fail map[int64]error
}

func (f *fakeSender) submit(_ context.Context, chat int64, html string) <-chan error {
	ch := make(chan error, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[chat]; err != nil {
		ch <- err
		return ch
	}
	f.sent = append(f.sent, sentMsg{chat, html})
	ch <- nil
	return ch
}

var testNow = time.Date(2026, 10, 31, 9, 4, 40, 0, time.UTC)

func newTestNotifier() (*Notifier, *memStore, *fakeSender) {
	c, _ := geo.FindCity("Balıkesir")
	van, _ := geo.FindCity("Van")
	ms := &memStore{log: map[string]float64{}, subs: []store.Subscription{
		{ID: 1, ChatID: 100, City: c.Name, Lat: c.Lat, Lon: c.Lon, RadiusKm: 100, MinMag: 3.5}, // Ayşe
		{ID: 2, ChatID: 200, City: c.Name, Lat: c.Lat, Lon: c.Lon, RadiusKm: 100, MinMag: 4.5}, // yüksek eşik
		{ID: 3, ChatID: 300, City: van.Name, Lat: van.Lat, Lon: van.Lon, RadiusKm: 200, MinMag: 3.0},
	}}
	fs := &fakeSender{fail: map[int64]error{}}
	n := &Notifier{
		Store: ms, Submit: fs.submit, MaxAge: time.Hour,
		Now: func() time.Time { return testNow },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return n, ms, fs
}

func update(mag float64, version int, prev *float64) normalize.EventUpdate {
	ev := sindirgi(mag)
	ev.Version = version
	ev.FirstReportedAt = testNow.Add(-3 * time.Second)
	return normalize.EventUpdate{Event: ev, PrevMagnitude: prev, IsNew: prev == nil}
}

func TestNotifierScenario1And2(t *testing.T) {
	n, _, fs := newTestNotifier()
	ctx := context.Background()

	// Senaryo 1: ilk sürüm Kandilli'den ML 4,3. Yalnızca Ayşe (eşik 3,5) alır.
	if err := n.Handle(ctx, update(4.3, 1, nil)); err != nil {
		t.Fatal(err)
	}
	if len(fs.sent) != 1 || fs.sent[0].chat != 100 {
		t.Fatalf("yalnızca Ayşe'ye tek mesaj gitmeli: %+v", fs.sent)
	}
	// Aynı mesaj yeniden teslim edilir (at-least-once): yeni gönderim yok.
	if err := n.Handle(ctx, update(4.3, 1, nil)); err != nil {
		t.Fatal(err)
	}
	// Senaryo 2: 4,3 -> 4,1 düzeltmesi: yeni bildirim yok.
	if err := n.Handle(ctx, update(4.1, 2, ptr(4.3))); err != nil {
		t.Fatal(err)
	}
	if len(fs.sent) != 1 {
		t.Fatalf("Senaryo 2 yeni bildirim üretmemeli: %+v", fs.sent)
	}
}

func TestNotifierUpdateAndThresholdCrossing(t *testing.T) {
	n, _, fs := newTestNotifier()
	ctx := context.Background()
	n.Handle(ctx, update(4.1, 1, nil)) // Ayşe: initial
	// 4,1 -> 4,6: Ayşe'ye güncelleme (fark 0,5); yüksek eşikli abone eşiği ilk kez geçti -> initial.
	if err := n.Handle(ctx, update(4.6, 2, ptr(4.1))); err != nil {
		t.Fatal(err)
	}
	if len(fs.sent) != 3 {
		t.Fatalf("3 mesaj bekleniyordu: %+v", fs.sent)
	}
	if fs.sent[1].chat != 100 || !strings.Contains(fs.sent[1].html, "Güncelleme") || !strings.Contains(fs.sent[1].html, "önceki bildirim: 4,1") {
		t.Errorf("Ayşe'ye güncelleme bekleniyordu: %q", fs.sent[1].html)
	}
	if fs.sent[2].chat != 200 || strings.Contains(fs.sent[2].html, "Güncelleme") {
		t.Errorf("yüksek eşikli aboneye ilk bildirim bekleniyordu: %q", fs.sent[2].html)
	}
	// 4,6 -> 5,2: Ayşe zaten güncelleme aldı; 200 numaralı abone 4,6'dan 0,6 fark -> güncelleme.
	n.Handle(ctx, update(5.2, 3, ptr(4.6)))
	if len(fs.sent) != 4 || fs.sent[3].chat != 200 {
		t.Fatalf("yalnızca 200'e güncelleme: %+v", fs.sent)
	}
}

func TestNotifierSendFailureRollsBack(t *testing.T) {
	n, ms, fs := newTestNotifier()
	ctx := context.Background()
	fs.fail[100] = errors.New("ağ hatası")
	if err := n.Handle(ctx, update(4.1, 1, nil)); err == nil {
		t.Fatal("geçici hata dönmeli ki mesaj ACK'lenmesin")
	}
	if len(ms.log) != 0 {
		t.Fatalf("başarısız gönderimin kaydı geri alınmalı: %v", ms.log)
	}
	// Yeniden teslim: bu kez başarılı.
	delete(fs.fail, 100)
	if err := n.Handle(ctx, update(4.1, 1, nil)); err != nil {
		t.Fatal(err)
	}
	if len(fs.sent) != 1 || len(ms.log) != 1 {
		t.Fatalf("yeniden denemede gönderilmeli: sent=%v log=%v", fs.sent, ms.log)
	}
}

func TestNotifierPermanentFailureKeepsRecord(t *testing.T) {
	n, ms, fs := newTestNotifier()
	fs.fail[100] = &APIError{Code: 403, Description: "Forbidden: bot was blocked by the user"}
	if err := n.Handle(context.Background(), update(4.1, 1, nil)); err != nil {
		t.Fatalf("kalıcı hata mesajı tıkamamalı: %v", err)
	}
	if len(ms.log) != 1 {
		t.Fatalf("kayıt kalmalı ki sonsuz deneme olmasın: %v", ms.log)
	}
}

func TestNotifierSkipsOldAndStoreErrors(t *testing.T) {
	n, ms, fs := newTestNotifier()
	old := update(5.0, 1, nil)
	old.Event.OriginTime = testNow.Add(-2 * time.Hour)
	if err := n.Handle(context.Background(), old); err != nil || len(fs.sent) != 0 {
		t.Fatalf("eski olay için bildirim olmamalı: err=%v sent=%v", err, fs.sent)
	}
	ms.failAff = errors.New("db yok")
	if err := n.Handle(context.Background(), update(4.1, 1, nil)); err == nil {
		t.Fatal("veritabanı hatası dönmeli (yeniden denensin)")
	}
}
