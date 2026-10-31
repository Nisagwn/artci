package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/summary"
	"github.com/nisaguven/artci/migrations"
)

func TestSummariesIntegration(t *testing.T) {
	dsn := os.Getenv("ARTCI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ARTCI_TEST_POSTGRES_DSN yok")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close) // defer değil: aşağıdaki temizlikler havuz kapanmadan çalışmalı
	if err := s.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	id := newUUID(t)
	day := time.Date(2099, 1, 2, 10, 0, 0, 0, time.UTC) // başka testlerle çakışmasın
	t.Cleanup(func() {
		s.Pool.Exec(ctx, `DELETE FROM events WHERE id=$1`, id)
		s.Pool.Exec(ctx, `DELETE FROM llm_spend WHERE day=$1::date`, day.Format("2006-01-02"))
	})
	if _, err := s.Pool.Exec(ctx, `INSERT INTO events (id, origin_time, location, magnitude, version)
		VALUES ($1, now(), ST_SetSRID(ST_MakePoint(28, 39), 4326)::geography, 4.1, 2)`, id); err != nil {
		t.Fatal(err)
	}

	if has, err := s.HasSummary(ctx, id, 1); err != nil || has {
		t.Fatalf("başta özet yok: %v %v", has, err)
	}
	rec := summary.Record{EventID: id, EventVersion: 2, Text: "ilk", Generator: "template", Validated: true}
	if err := s.SaveSummary(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.Text = "ikinci"
	if err := s.SaveSummary(ctx, rec); err != nil {
		t.Fatal("ikinci yazım hata vermemeli (idempotent):", err)
	}
	var text string
	s.Pool.QueryRow(ctx, `SELECT text FROM summaries WHERE event_id=$1`, id).Scan(&text)
	if text != "ilk" {
		t.Errorf("ilk yazım korunmalı, %q", text)
	}
	for v, want := range map[int]bool{1: true, 2: true, 3: false} {
		if has, _ := s.HasSummary(ctx, id, v); has != want {
			t.Errorf("HasSummary(v%d)=%v, beklenen %v", v, has, want)
		}
	}
	// api, doğrulanmış özeti olay görünümünde döner.
	d, err := s.EventDetail(ctx, id)
	if err != nil || d == nil || d.Summary == nil || *d.Summary != "ilk" {
		t.Errorf("EventDetail özeti dönmeli: %+v %v", d, err)
	}

	if usd, err := s.SpentOn(ctx, day); err != nil || usd != 0 {
		t.Fatalf("başta 0: %v %v", usd, err)
	}
	s.AddSpend(ctx, day, 0.25)
	s.AddSpend(ctx, day, 0.5)
	if usd, _ := s.SpentOn(ctx, day); usd != 0.75 {
		t.Errorf("toplam 0,75 olmalı: %v", usd)
	}
}
