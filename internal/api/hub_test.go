package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nisaguven/artci/internal/normalize"
)

func upd(id string, version int, mag float64, isNew bool) normalize.EventUpdate {
	return normalize.EventUpdate{
		Event: normalize.Event{ID: id, Version: version, Magnitude: mag, OriginTime: testNow},
		IsNew: isNew,
	}
}

func testClient(buf int) *client {
	return &client{send: make(chan []byte, buf), quit: make(chan struct{})}
}

func decodeBatch(t *testing.T, b []byte) BatchMessage {
	t.Helper()
	var m BatchMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "batch" {
		t.Fatalf("tür: %q", m.Type)
	}
	return m
}

func TestHubMergesVersions(t *testing.T) {
	h := NewHub(HubConfig{})
	c := testClient(4)
	if err := h.register(c); err != nil {
		t.Fatal(err)
	}
	h.Add(upd("a", 1, 4.3, true))
	h.Add(upd("a", 3, 4.1, false))
	h.Add(upd("a", 2, 4.2, false)) // sırası bozuk gelen eski sürüm kazanmamalı
	h.Add(upd("b", 1, 2.0, false))
	h.Flush()

	if len(c.send) != 1 {
		t.Fatalf("tek toplu mesaj beklenirdi, %d", len(c.send))
	}
	m := decodeBatch(t, <-c.send)
	if len(m.Events) != 2 {
		t.Fatalf("olay sayısı: %d", len(m.Events))
	}
	byID := map[string]WireEvent{}
	for _, e := range m.Events {
		byID[e.ID] = e
	}
	a := byID["a"]
	if a.Version != 3 || a.Magnitude != 4.1 || !a.IsNew {
		t.Fatalf("a birleşimi yanlış: %+v", a)
	}
	if byID["b"].IsNew {
		t.Fatal("b yeni değil")
	}

	// Boş aralıkta mesaj gönderilmez.
	h.Flush()
	if len(c.send) != 0 {
		t.Fatal("boş flush mesaj göndermemeli")
	}
}

func TestHubDropsSlowClient(t *testing.T) {
	h := NewHub(HubConfig{BufferSize: 2})
	slow, fast := testClient(2), testClient(2)
	h.register(slow)
	h.register(fast)

	for i := 1; i <= 3; i++ {
		h.Add(upd("a", i, 3, false))
		h.Flush()
		<-fast.send // hızlı istemci okumaya devam ediyor
	}
	select {
	case <-slow.quit:
	default:
		t.Fatal("tamponu dolan istemci atılmalıydı")
	}
	if slow.code != websocket.StatusPolicyViolation {
		t.Fatalf("kapanış kodu: %v", slow.code)
	}
	select {
	case <-fast.quit:
		t.Fatal("hızlı istemci etkilenmemeli")
	default:
	}
	if h.Clients() != 1 {
		t.Fatalf("istemci sayısı: %d", h.Clients())
	}
}

func TestHubMaxClientsAndClose(t *testing.T) {
	h := NewHub(HubConfig{MaxClients: 1})
	c := testClient(1)
	if err := h.register(c); err != nil {
		t.Fatal(err)
	}
	if err := h.register(testClient(1)); err != errHubFull {
		t.Fatalf("sınır: %v", err)
	}
	h.Close()
	<-c.quit
	if c.code != websocket.StatusGoingAway {
		t.Fatalf("kapanış kodu: %v", c.code)
	}
	if err := h.register(testClient(1)); err != errHubClosed {
		t.Fatalf("kapalı hub: %v", err)
	}
}

// Uçtan uca: gerçek HTTP sunucusu + WebSocket istemcisi.
func TestHubWebSocketEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hub := NewHub(HubConfig{Interval: 20 * time.Millisecond, MaxClients: 1})
	go hub.Run(ctx)
	srv := &Server{Store: &fakeStore{}, WS: hub}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for hub.Clients() != 1 {
		time.Sleep(5 * time.Millisecond)
	}

	// Sınır dolu: ikinci bağlantı yükseltilmeden reddedilir.
	if _, resp, err := websocket.Dial(ctx, url, nil); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ikinci bağlantı reddedilmeliydi: %v", err)
	}

	// Aynı aralıkta gelen üç sürüm tek mesajda, en yüksek sürümle gelir.
	hub.Add(upd("e1", 1, 4.3, true))
	hub.Add(upd("e1", 2, 4.2, false))
	hub.Add(upd("e1", 3, 4.1, false))
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeBatch(t, b)
	if len(m.Events) != 1 || m.Events[0].Version != 3 || !m.Events[0].IsNew {
		t.Fatalf("mesaj: %+v", m)
	}

	// Kapanışta istemci "going away" alır ve bağlantı goroutine'i biter.
	hub.Close()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("kapanış: %v", err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	if err := hub.Wait(wctx); err != nil {
		t.Fatalf("goroutine'ler bitmedi: %v", err)
	}
}
