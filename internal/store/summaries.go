package store

import (
	"context"
	"time"

	"github.com/nisaguven/artci/internal/summary"
)

// HasSummary: bu olayın bu ya da daha yeni bir sürümü için özet var mı?
// Daha yeni sürümün özeti varsa eski sürüm için LLM'e para harcamayız.
func (s *Store) HasSummary(ctx context.Context, eventID string, version int) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM summaries WHERE event_id = $1 AND event_version >= $2)`,
		eventID, version).Scan(&ok)
	return ok, err
}

// SaveSummary idempotenttir: aynı (olay, sürüm) ikinci kez yazılmaz.
func (s *Store) SaveSummary(ctx context.Context, r summary.Record) error {
	_, err := s.Pool.Exec(ctx, `
INSERT INTO summaries (event_id, event_version, text, generator, validated)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (event_id, event_version) DO NOTHING`,
		r.EventID, r.EventVersion, r.Text, r.Generator, r.Validated)
	return err
}

// SpentOn, verilen gündeki toplam LLM harcamasıdır (USD).
func (s *Store) SpentOn(ctx context.Context, day time.Time) (float64, error) {
	var usd float64
	err := s.Pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT usd::float8 FROM llm_spend WHERE day = $1::date), 0)`,
		day.Format("2006-01-02")).Scan(&usd)
	return usd, err
}

// AddSpend, günün harcamasına ekler.
func (s *Store) AddSpend(ctx context.Context, day time.Time, usd float64) error {
	_, err := s.Pool.Exec(ctx, `
INSERT INTO llm_spend (day, usd, calls) VALUES ($1::date, $2, 1)
ON CONFLICT (day) DO UPDATE SET usd = llm_spend.usd + EXCLUDED.usd, calls = llm_spend.calls + 1`,
		day.Format("2006-01-02"), usd)
	return err
}

var _ summary.Store = (*Store)(nil)
