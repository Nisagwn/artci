package summary

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
)

// Record, summaries tablosundaki bir satırdır.
type Record struct {
	EventID      string
	EventVersion int
	Text         string
	Generator    string // "llm:<model>" | "template"
	Validated    bool
}

// Store, servisin ihtiyaç duyduğu kalıcılık işlemleridir (store.Store uygular).
type Store interface {
	// HasSummary: bu olayın bu ya da daha yeni bir sürümü için özet var mı?
	HasSummary(ctx context.Context, eventID string, version int) (bool, error)
	// SaveSummary idempotenttir: aynı (olay, sürüm) için ikinci yazım etkisizdir.
	SaveSummary(ctx context.Context, r Record) error
	SpentOn(ctx context.Context, day time.Time) (float64, error)
	AddSpend(ctx context.Context, day time.Time, usd float64) error
}

// Config, servis ayarlarıdır.
type Config struct {
	MinMagnitude float64       // bunun altındaki olaylara özet üretilmez (Plan §11: 3,5)
	DailyCapUSD  float64       // günlük LLM harcama tavanı; aşılınca yalnızca şablon
	LLMTimeout   time.Duration // tek çağrı için üst sınır
}

// DefaultConfig, Plan §11'deki değerlerdir.
func DefaultConfig() Config {
	return Config{MinMagnitude: 3.5, DailyCapUSD: 1, LLMTimeout: 15 * time.Second}
}

// Service, events.updated mesajlarından özet üretir.
type Service struct {
	Store Store
	LLM   Generator // nil ise yalnızca şablon
	Cfg   Config
	Log   *slog.Logger
	Now   func() time.Time
}

// Outcome, tek bir olay için ne yapıldığıdır.
type Outcome string

const (
	OutcomeSkippedSmall  Outcome = "skipped_small"  // eşiğin altında
	OutcomeSkippedCached Outcome = "skipped_cached" // bu sürüm için özet zaten var
	OutcomeLLM           Outcome = "llm_ok"
	OutcomeFallback      Outcome = "fallback" // şablon (LLM yok, hata, red ya da bütçe)
)

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handle, tek bir olay güncellemesini işler. Dönen hata geçicidir (DB);
// çağıran mesajı ACK'lememeli. LLM hataları hata değildir: şablona düşülür.
func (s *Service) Handle(ctx context.Context, u normalize.EventUpdate) (Outcome, error) {
	ev := u.Event
	if ev.Magnitude < s.Cfg.MinMagnitude {
		return OutcomeSkippedSmall, nil
	}
	has, err := s.Store.HasSummary(ctx, ev.ID, ev.Version)
	if err != nil {
		return "", err
	}
	if has {
		return OutcomeSkippedCached, nil
	}
	f := BuildFacts(ev)
	lg := s.log().With("event_id", ev.ID, "version", ev.Version)

	if text, gen, ok := s.tryLLM(ctx, f, lg); ok {
		if err := s.Store.SaveSummary(ctx, Record{ev.ID, ev.Version, text, gen, true}); err != nil {
			return "", err
		}
		metrics.SummaryResults.WithLabelValues("llm_ok").Inc()
		return OutcomeLLM, nil
	}

	text := Template(f)
	verr := Validate(text, f)
	if verr != nil {
		// Şablon testlerle güvence altında; yine de olursa yayınlamayız.
		lg.Error("şablon özet doğrulamadan geçemedi", "err", verr, "text", text)
	}
	if err := s.Store.SaveSummary(ctx, Record{ev.ID, ev.Version, text, "template", verr == nil}); err != nil {
		return "", err
	}
	metrics.SummaryResults.WithLabelValues("fallback").Inc()
	return OutcomeFallback, nil
}

// tryLLM, LLM özetini dener. ok=false ise şablona düşülmelidir.
func (s *Service) tryLLM(ctx context.Context, f Facts, lg *slog.Logger) (string, string, bool) {
	if s.LLM == nil {
		return "", "", false
	}
	day := s.now().In(istanbul)
	spent, err := s.Store.SpentOn(ctx, day)
	if err != nil {
		lg.Warn("harcama okunamadı; LLM atlandı", "err", err)
		return "", "", false
	}
	if spent >= s.Cfg.DailyCapUSD {
		lg.Info("günlük LLM tavanına ulaşıldı; şablon kullanılıyor", "spent_usd", spent, "cap_usd", s.Cfg.DailyCapUSD)
		return "", "", false
	}

	timeout := s.Cfg.LLMTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	raw, cost, err := s.LLM.Generate(cctx, f)
	cancel()
	if cost > 0 {
		metrics.LLMCost.Add(cost)
		if err := s.Store.AddSpend(ctx, day, cost); err != nil {
			lg.Warn("harcama kaydedilemedi", "err", err, "usd", cost)
		}
	}
	if err != nil {
		lg.Warn("LLM çağrısı başarısız; şablon kullanılıyor", "err", err)
		return "", "", false
	}
	text, err := ParseModelOutput(raw)
	if err == nil {
		err = Validate(text, f)
	}
	if err != nil {
		code := "unknown"
		if r, ok := IsRejection(err); ok {
			code = r.Code
		}
		metrics.SummaryResults.WithLabelValues("rejected").Inc()
		// Reddedilen yanıt loglanır: red-team setini büyütmek için ham malzeme.
		lg.Warn("LLM özeti reddedildi", "code", code, "err", err, "raw", raw)
		return "", "", false
	}
	return text, s.LLM.Name(), true
}

// ErrNoEvent, mesajda olay yoksa döner (kalıcı hata).
var ErrNoEvent = errors.New("mesajda olay yok")
