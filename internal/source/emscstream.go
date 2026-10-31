package source

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
)

// GapFiller, belirli bir oluş zamanından bu yana kayıtları döner (EMSC FDSN).
type GapFiller interface {
	Since(ctx context.Context, since time.Time) (Result, error)
}

// EMSCStream, SeismicPortal WebSocket akışını dinler (Plan §9).
//
// Bağlantı koparsa Backoff ile yeniden bağlanır. Her (yeniden) bağlanmadan
// SONRA, kopma anından GapMargin geriye kadar FDSN ile boşluk doldurulur.
// Sıra önemli: önce bağlanıp sonra doldurmak, doldurma ile akışın başlaması
// arasına düşen olayın kaybolmasını önler; iki yoldan gelen aynı kayıt
// Emitter ve merger tarafından tekilleştirilir.
//
// GapMargin, oluş zamanı ile EMSC'nin olayı yayınlaması arasındaki gecikmeyi
// kapsamalıdır: FDSN'de "start" oluş zamanına göre filtreler.
type EMSCStream struct {
	URL             string // boşsa EMSCWebSocketURL
	Filler          GapFiller
	Emitter         *Emitter
	Backoff         Backoff
	GapMargin       time.Duration // varsayılan 15 dk
	InitialLookback time.Duration // ilk bağlantıda geriye bakış (varsayılan 1 saat)
	PingInterval    time.Duration // ölü bağlantı tespiti (varsayılan 30 sn)
	UserAgent       string
	Now             func() time.Time
}

func (s *EMSCStream) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *EMSCStream) defaults() {
	if s.URL == "" {
		s.URL = EMSCWebSocketURL
	}
	if s.GapMargin == 0 {
		s.GapMargin = 15 * time.Minute
	}
	if s.InitialLookback == 0 {
		s.InitialLookback = time.Hour
	}
	if s.PingInterval == 0 {
		s.PingInterval = 30 * time.Second
	}
	if s.UserAgent == "" {
		s.UserAgent = UserAgent
	}
}

// Run, ctx iptal edilene kadar akışı dinler.
func (s *EMSCStream) Run(ctx context.Context) {
	s.defaults()
	since := s.now().Add(-s.InitialLookback)
	failures := 0
	for ctx.Err() == nil {
		conn, _, err := websocket.Dial(ctx, s.URL, &websocket.DialOptions{
			HTTPHeader: http.Header{"User-Agent": {s.UserAgent}},
		})
		if err != nil {
			failures++
			metrics.SourceUp.WithLabelValues(normalize.SourceEMSC).Set(0)
			wait := s.Backoff.Next(failures)
			slog.Warn("emsc: bağlanılamadı", "err", err, "attempt", failures, "retry_in", wait.Round(time.Millisecond))
			sleep(ctx, wait)
			continue
		}
		failures = 0
		metrics.SourceUp.WithLabelValues(normalize.SourceEMSC).Set(1)
		metrics.SourceLastSuccess.WithLabelValues(normalize.SourceEMSC).SetToCurrentTime()
		slog.Info("emsc: bağlandı", "gap_fill_since", since.UTC())

		err = s.serve(ctx, conn, since)
		// Kopma anından itibaren bir sonraki bağlantıda doldurulacak.
		since = s.now().Add(-s.GapMargin)
		metrics.SourceUp.WithLabelValues(normalize.SourceEMSC).Set(0)
		if ctx.Err() != nil {
			conn.Close(websocket.StatusNormalClosure, "kapanıyor")
			break
		}
		conn.Close(websocket.StatusGoingAway, "yeniden bağlanılıyor")
		slog.Warn("emsc: bağlantı koptu", "err", err)
		sleep(ctx, s.Backoff.Next(1))
	}
	slog.Info("toplayıcı durdu", "source", normalize.SourceEMSC)
}

// serve, tek bir bağlantının ömrüdür. Dönen hata, bağlantının neden bittiğidir.
func (s *EMSCStream) serve(ctx context.Context, conn *websocket.Conn, since time.Time) error {
	conn.SetReadLimit(1 << 20)
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go s.fillGap(cctx, since)
	go s.keepAlive(cctx, conn, cancel)

	em := s.Emitter
	for {
		_, msg, err := conn.Read(cctx)
		if err != nil {
			if cause := context.Cause(cctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return err
		}
		now := s.now()
		metrics.SourceLastSuccess.WithLabelValues(normalize.SourceEMSC).SetToCurrentTime()
		r, err := ParseEMSCMessage(msg, now)
		var res Result
		switch {
		case errors.Is(err, ErrOutOfRegion):
			continue
		case err != nil:
			res.Bad = []BadRecord{{Raw: string(msg), Reason: err.Error()}}
		default:
			res = finish([]normalize.Report{r}, nil)
		}
		if _, err := em.Emit(ctx, res); err != nil {
			// Akıştan gelen mesaj tekrar gelmez. Bağlantıyı bırakıp yeniden
			// bağlanınca boşluk doldurma bu olayı FDSN'den geri getirir.
			return errors.Join(errors.New("yayın başarısız"), err)
		}
	}
}

// fillGap, FDSN'den since anından bu yana kayıtları çekip yayınlar.
// Başarısız olursa bağlantı sürdükçe backoff ile yeniden dener.
func (s *EMSCStream) fillGap(ctx context.Context, since time.Time) {
	if s.Filler == nil {
		return
	}
	for attempt := 1; ctx.Err() == nil; attempt++ {
		res, err := s.Filler.Since(ctx, since)
		if err == nil {
			st, err2 := s.Emitter.Emit(ctx, res)
			if err2 == nil {
				slog.Info("emsc: boşluk dolduruldu", "since", since.UTC(), "published", st.Published, "skipped", st.Skipped)
				return
			}
			err = err2
		}
		if ctx.Err() != nil {
			return
		}
		wait := s.Backoff.Next(attempt)
		slog.Warn("emsc: boşluk doldurulamadı", "err", err, "attempt", attempt, "retry_in", wait.Round(time.Millisecond))
		sleep(ctx, wait)
	}
}

// keepAlive, düzenli ping atar; yanıt gelmezse bağlantıyı düşürür.
// EMSC sessiz kalabilir; TCP yarı açık kalırsa Read sonsuza dek beklerdi.
func (s *EMSCStream) keepAlive(ctx context.Context, conn *websocket.Conn, cancel context.CancelCauseFunc) {
	t := time.NewTicker(s.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, s.PingInterval/2+time.Second)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil && ctx.Err() == nil {
				cancel(errors.Join(errors.New("ping yanıtsız"), err))
				return
			}
			// Sakin dönemde mesaj gelmez; canlı bağlantı da "kaynak sağlıklı"
			// demektir. "Kaynağa 15 dk ulaşılamadı" alarmı buna dayanır.
			metrics.SourceLastSuccess.WithLabelValues(normalize.SourceEMSC).SetToCurrentTime()
		}
	}
}
