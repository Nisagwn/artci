// Package merger, reports.raw'dan gelen raporları olaylara dönüştürür
// (Plan §8). Karar mantığı match paketinde (saf); burada yalnızca akış var:
// upsert → eşleştir → olayı yaz → commit → yayınla.
//
// Tek merger varsayımı (Plan §6): Handle aynı anda tek bir mesaj için çağrılır.
// İki merger paralel çalışsaydı aynı depremin iki raporu birbirini görmeden
// iki ayrı olay açabilirdi.
package merger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/nisaguven/artci/internal/match"
	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
)

// Tx, tek transaction içindeki depo işlemleridir. *store.ReportTx bunu sağlar;
// testlerde bellek içi bir sahte kullanılır.
type Tx interface {
	UpsertReport(ctx context.Context, r normalize.Report) (store.UpsertResult, error)
	FindCandidateEvents(ctx context.Context, r normalize.Report, window time.Duration, radiusKm float64) ([]match.Candidate, error)
	GetEvent(ctx context.Context, id string) (normalize.Event, error)
	ReportsForEvent(ctx context.Context, eventID string) ([]normalize.Report, error)
	CreateEvent(ctx context.Context, ev normalize.Event) error
	UpdateEvent(ctx context.Context, ev normalize.Event) (int, error)
	AttachReport(ctx context.Context, reportID int64, eventID string) error
}

// Store, merger'ın depoya bakışıdır.
type Store interface {
	// InTx, fn'i tek transaction'da çalıştırır; fn hata dönerse hepsi geri alınır.
	InTx(ctx context.Context, fn func(Tx) error) error
	// UnpublishedEvents / MarkPublished: commit sonrası yayın garantisi (002 outbox).
	UnpublishedEvents(ctx context.Context) ([]normalize.EventUpdate, error)
	MarkPublished(ctx context.Context, eventID string, version int, magnitude float64) error
}

// Publisher, events.updated akışına yazar.
type Publisher interface {
	Publish(ctx context.Context, u normalize.EventUpdate) error
}

// Decision, artci_merge_decisions_total etiketidir (Plan §12).
type Decision string

const (
	DecisionNewEvent Decision = "new_event" // yeni olay açıldı
	DecisionAttached Decision = "attached"  // mevcut olaya bağlandı
	DecisionUpdate   Decision = "update"    // kaynak kendi raporunu düzeltti
	DecisionNoop     Decision = "noop"      // aynı mesaj tekrar geldi
)

// Service, merger'ın kendisidir.
type Service struct {
	Store Store
	Pub   Publisher
	Cfg   match.Config
	NewID func() string // test için değiştirilebilir; varsayılan uuid
	Log   *slog.Logger
}

// New, varsayılanlarla bir Service kurar.
func New(st Store, pub Publisher, cfg match.Config) *Service {
	return &Service{Store: st, Pub: pub, Cfg: cfg, NewID: uuid.NewString, Log: slog.Default()}
}

// ErrPublish, DB commit edildikten sonra yayının başarısız olduğunu söyler.
// Geçicidir: mesaj ACK'lenmez, yeniden işlendiğinde noop olur ama
// PublishPending eksik yayını tamamlar.
var ErrPublish = errors.New("events.updated yayını başarısız")

// Handle, tek bir raporu işler. Hata dönerse hiçbir şey commit edilmemiştir
// (ya da yalnızca yayın eksiktir, bkz. ErrPublish); çağıran mesajı ACK'lememeli.
func (s *Service) Handle(ctx context.Context, r normalize.Report) (Decision, error) {
	var dec Decision
	err := s.Store.InTx(ctx, func(tx Tx) error {
		var err error
		dec, err = s.apply(ctx, tx, r)
		return err
	})
	if err != nil {
		return "", err
	}
	metrics.MergeDecisions.WithLabelValues(string(dec)).Inc()
	// Yayın commit'ten SONRA: tüketiciler DB'de henüz olmayan bir olayı görmesin.
	if err := s.PublishPending(ctx); err != nil {
		return dec, err
	}
	return dec, nil
}

// PublishPending, sürümü yayınlanmamış tüm olayları yayınlar. Hem her mesajdan
// sonra hem açılışta çağrılır; böylece "commit oldu ama yayın olmadı" penceresi
// kendiliğinden kapanır. Tüketiciler at-least-once'a hazır olmalı (Plan §9).
func (s *Service) PublishPending(ctx context.Context) error {
	ups, err := s.Store.UnpublishedEvents(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPublish, err)
	}
	for _, u := range ups {
		if err := s.Pub.Publish(ctx, u); err != nil {
			return fmt.Errorf("%w: %v", ErrPublish, err)
		}
		if err := s.Store.MarkPublished(ctx, u.Event.ID, u.Event.Version, u.Event.Magnitude); err != nil {
			// Yayın gitti ama işaretlenmedi: bir dahaki sefere tekrar gider (zararsız).
			return fmt.Errorf("%w: %v", ErrPublish, err)
		}
	}
	return nil
}

func (s *Service) apply(ctx context.Context, tx Tx, r normalize.Report) (Decision, error) {
	res, err := tx.UpsertReport(ctx, r)
	if err != nil {
		return "", err
	}
	switch {
	case res.EventID == "":
		// Yeni rapor. (Bağsız eski rapor normalde olamaz; olsa da aynı yoldan geçer.)
		return s.place(ctx, tx, r, res.ReportID)

	case res.Status == store.ReportUnchanged:
		return DecisionNoop, nil

	default: // ReportUpdated veya ReportInserted+bağlı (olamaz) -> güncelleme
		return DecisionUpdate, s.reconsider(ctx, tx, r, res)
	}
}

// reconsider: kaynak raporunu düzeltti. Çoğu zaman olayı yeniden hesaplamak
// yeter. Ama düzeltme raporu olayından koparabilir (ör. konum 200 km kaydı):
// o zaman rapor olaydan ayrılır, eski olay kalan raporlarla yeniden hesaplanır
// ve rapor yeni gelmiş gibi yeniden eşleştirilir (başka olaya bağlanabilir ya da
// yeni olay açabilir). Bkz. docs/MATCHING.md "Olay bölünmesi".
func (s *Service) reconsider(ctx context.Context, tx Tx, r normalize.Report, res store.UpsertResult) error {
	reports, err := tx.ReportsForEvent(ctx, res.EventID)
	if err != nil {
		return err
	}
	var others []normalize.Report
	for _, o := range reports {
		if o.Source != r.Source || o.SourceEventID != r.SourceEventID {
			others = append(others, o)
		}
	}
	// Tek başınaysa olay raporla birlikte "taşınır"; bölünecek bir şey yok.
	if len(others) > 0 {
		fit := s.Cfg.Decide(r, []match.Candidate{match.CandidateOf(res.EventID, others)})
		if fit.Action != match.Attach {
			s.Log.Info("rapor güncellemesi olayı böldü",
				"source", r.Source, "source_event_id", r.SourceEventID, "event_id", res.EventID)
			prev, err := tx.GetEvent(ctx, res.EventID) // koparmadan önce: kaynak listesi değişimi görünsün
			if err != nil {
				return err
			}
			if err := tx.AttachReport(ctx, res.ReportID, ""); err != nil {
				return err
			}
			if _, err := s.recomputeFrom(ctx, tx, prev); err != nil {
				return err
			}
			_, err = s.place(ctx, tx, r, res.ReportID)
			return err
		}
	}
	_, err = s.recompute(ctx, tx, res.EventID)
	return err
}

// place: Plan §8 adım 2-5. Adayları bul, karar ver, bağla ya da yeni olay aç.
func (s *Service) place(ctx context.Context, tx Tx, r normalize.Report, reportID int64) (Decision, error) {
	cands, err := tx.FindCandidateEvents(ctx, r, s.Cfg.TimeWindow, s.Cfg.MaxDistanceKm)
	if err != nil {
		return "", err
	}
	d := s.Cfg.Decide(r, cands)
	if d.Action == match.Attach {
		// Önceki hâl bağlamadan ÖNCE okunur. Kaynak listesi raporlardan
		// türetildiği için sonra okunursa yeni kaynağı zaten içerir ve yalnızca
		// kaynağı değişen olay yeni sürüm almaz, yayınlanmazdı.
		prev, err := tx.GetEvent(ctx, d.EventID)
		if err != nil {
			return "", err
		}
		if err := tx.AttachReport(ctx, reportID, d.EventID); err != nil {
			return "", err
		}
		if _, err := s.recomputeFrom(ctx, tx, prev); err != nil {
			return "", err
		}
		return DecisionAttached, nil
	}

	ev := match.Resolve([]normalize.Report{r})
	ev.ID = s.NewID()
	if err := tx.CreateEvent(ctx, ev); err != nil {
		return "", err
	}
	if err := tx.AttachReport(ctx, reportID, ev.ID); err != nil {
		return "", err
	}
	return DecisionNewEvent, nil
}

// recompute: olayın gösterilen değerlerini raporlarından yeniden hesaplar;
// yalnızca anlamlı değişiklik varsa yazar (version+1). Değişiklik yoksa sürüm
// artmaz, dolayısıyla yayın da olmaz.
func (s *Service) recompute(ctx context.Context, tx Tx, eventID string) (bool, error) {
	prev, err := tx.GetEvent(ctx, eventID)
	if err != nil {
		return false, err
	}
	return s.recomputeFrom(ctx, tx, prev)
}

// recomputeFrom, prev'i (üyelik değişmeden önceki hâl) karşılaştırma tabanı alır.
func (s *Service) recomputeFrom(ctx context.Context, tx Tx, prev normalize.Event) (bool, error) {
	eventID := prev.ID
	reports, err := tx.ReportsForEvent(ctx, eventID)
	if err != nil {
		return false, err
	}
	if len(reports) == 0 {
		// Olamaz: yalnızca başka üyesi olan olaydan rapor koparıyoruz.
		return false, fmt.Errorf("olay %s raporsuz kaldı", eventID)
	}
	next := match.Resolve(reports)
	next.ID = eventID
	if !match.EventChanged(prev, next) {
		return false, nil
	}
	_, err = tx.UpdateEvent(ctx, next)
	return err == nil, err
}
