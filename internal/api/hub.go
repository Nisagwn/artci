package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
)

// HubConfig, yayın merkezinin ayarlarıdır; sıfır değerler varsayılanla dolar.
type HubConfig struct {
	Interval     time.Duration // toplu gönderim aralığı (Plan §9: saniyede bir)
	BufferSize   int           // istemci başına bekleyen mesaj sınırı
	MaxClients   int           // aynı anda en fazla bağlantı
	WriteTimeout time.Duration // tek yazma için üst sınır
	PingInterval time.Duration // ölü bağlantıları ayıklamak için
}

func (c *HubConfig) defaults() {
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 8
	}
	if c.MaxClients <= 0 {
		c.MaxClients = 1000
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 5 * time.Second
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 30 * time.Second
	}
}

// WireEvent, istemciye giden olaydır.
type WireEvent struct {
	normalize.Event
	IsNew bool `json:"is_new"`
}

// BatchMessage, her aralıkta gönderilen tek mesajdır.
type BatchMessage struct {
	Type   string      `json:"type"` // "batch"
	Events []WireEvent `json:"events"`
}

var errHubFull = errors.New("bağlantı sınırı dolu")
var errHubClosed = errors.New("hub kapalı")

type client struct {
	send chan []byte
	quit chan struct{} // kapatılınca bağlantı sonlandırılır
	once sync.Once
	code websocket.StatusCode
	why  string
}

// kick, istemciyi bağlantıyı kapatmaya zorlar. Kapatmanın kendisi istemcinin
// goroutine'inde yapılır; böylece yayın döngüsü hiçbir ağ işleminde beklemez.
func (c *client) kick(code websocket.StatusCode, why string) {
	c.once.Do(func() {
		c.code, c.why = code, why
		close(c.quit)
	})
}

// Hub, olay güncellemelerini bellekte birleştirip bağlı tüm WebSocket
// istemcilerine aralıklarla toplu gönderir.
type Hub struct {
	cfg HubConfig

	mu      sync.Mutex
	pending map[string]WireEvent
	clients map[*client]struct{}
	closed  bool
	wg      sync.WaitGroup // açık bağlantı goroutine'leri (kapanışta beklemek için)
}

// NewHub, yeni bir hub oluşturur.
func NewHub(cfg HubConfig) *Hub {
	cfg.defaults()
	return &Hub{cfg: cfg, pending: map[string]WireEvent{}, clients: map[*client]struct{}{}}
}

// Add, bir güncellemeyi sıradaki toplu mesaja ekler. Aynı olayın ardışık
// sürümleri tek kayda iner ve en yüksek sürüm kazanır; deprem anındaki
// güncelleme fırtınası istemci başına saniyede tek mesaja dönüşür.
func (h *Hub) Add(u normalize.EventUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pending[u.Event.ID]
	// "Yeni" bilgisi kaybolmamalı: istemci ilk sürümü hiç görmemiş olabilir.
	isNew := u.IsNew || (ok && p.IsNew)
	if !ok || u.Event.Version >= p.Version {
		p.Event = u.Event
	}
	p.IsNew = isNew
	h.pending[u.Event.ID] = p
}

// Flush, bekleyen güncellemeleri tek mesaj olarak tüm istemcilere dağıtır.
// Tamponu dolu istemci beklenmez, bağlantısı kapatılır: yavaş tek bir
// istemci diğerlerini geciktirmemeli.
func (h *Hub) Flush() {
	h.mu.Lock()
	if len(h.pending) == 0 {
		h.mu.Unlock()
		return
	}
	evs := make([]WireEvent, 0, len(h.pending))
	for _, e := range h.pending {
		evs = append(evs, e)
	}
	h.pending = map[string]WireEvent{}
	h.mu.Unlock()

	// Kronolojik sıra: istemci listeye doğal sırayla eklesin.
	sort.Slice(evs, func(i, j int) bool {
		if !evs[i].OriginTime.Equal(evs[j].OriginTime) {
			return evs[i].OriginTime.Before(evs[j].OriginTime)
		}
		return evs[i].ID < evs[j].ID
	})
	msg, err := json.Marshal(BatchMessage{Type: "batch", Events: evs})
	if err != nil {
		slog.Error("ws batch marshal", "err", err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			delete(h.clients, c)
			c.kick(websocket.StatusPolicyViolation, "çok yavaş")
		}
	}
}

// Run, ctx bitene kadar her aralıkta Flush çağırır.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(h.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Flush()
		}
	}
}

// Clients, bağlı istemci sayısıdır.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *Hub) register(c *client) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errHubClosed
	}
	if len(h.clients) >= h.cfg.MaxClients {
		return errHubFull
	}
	h.clients[c] = struct{}{}
	return nil
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// Close, yeni bağlantıları reddeder ve mevcutlara "gidiyorum" kapanışı gönderir.
// http.Server.Shutdown ele geçirilmiş (hijack) bağlantıları kapatmadığı için gerekli.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		delete(h.clients, c)
		c.kick(websocket.StatusGoingAway, "sunucu kapanıyor")
	}
}

// Wait, açık bağlantı goroutine'lerinin bitmesini ctx süresince bekler.
func (h *Hub) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ServeHTTP, WebSocket bağlantısını kabul eder ve hub'a bağlar.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Clients() >= h.cfg.MaxClients {
		writeError(w, http.StatusServiceUnavailable, "bağlantı sınırı dolu, biraz sonra tekrar deneyin")
		return
	}
	// Accept varsayılan olarak Origin'in Host ile aynı olmasını ister (aynı köken);
	// başka sitelerin ziyaretçinin tarayıcısı üzerinden bağlanmasını engeller.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept yanıtı zaten yazdı
	}
	h.wg.Add(1)
	defer h.wg.Done()

	c := &client{send: make(chan []byte, h.cfg.BufferSize), quit: make(chan struct{})}
	if err := h.register(c); err != nil {
		conn.Close(websocket.StatusTryAgainLater, err.Error())
		return
	}
	metrics.WSClients.Inc()
	defer func() {
		h.unregister(c)
		metrics.WSClients.Dec()
	}()

	// İstemciden veri beklemiyoruz; CloseRead okumayı (pong ve kapanış
	// çerçeveleri dahil) arka planda yürütür ve bağlantı kopunca ctx'i bitirir.
	ctx := conn.CloseRead(context.Background())
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.CloseNow()
			return
		case <-c.quit:
			conn.Close(c.code, c.why)
			return
		case msg := <-c.send:
			if err := h.write(conn, msg); err != nil {
				conn.CloseNow()
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				conn.CloseNow()
				return
			}
		}
	}
}

func (h *Hub) write(conn *websocket.Conn, msg []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.WriteTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, msg)
}
