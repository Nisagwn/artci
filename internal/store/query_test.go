package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/nisaguven/artci/migrations"
)

func TestSortSources(t *testing.T) {
	s := []string{"emsc", "x", "afad", "kandilli"}
	sortSources(s)
	if want := []string{"afad", "kandilli", "emsc", "x"}; !slices.Equal(s, want) {
		t.Fatalf("got %v want %v", s, want)
	}
}

func newUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Gerçek PostGIS ile entegrasyon testi; DSN verilmezse atlanır.
func TestQueriesIntegration(t *testing.T) {
	dsn := os.Getenv("ARTCI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ARTCI_TEST_POSTGRES_DSN yok")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}

	// Başka testlerle çakışmasın diye gelecekte, benzersiz bir zaman penceresi kullanılır.
	base := time.Now().Add(1000 * time.Hour).Truncate(time.Second).UTC()
	big, small, old := newUUID(t), newUUID(t), newUUID(t)
	t.Cleanup(func() {
		for _, id := range []string{big, small, old} {
			s.Pool.Exec(ctx, `DELETE FROM reports WHERE event_id=$1`, id)
			s.Pool.Exec(ctx, `DELETE FROM events WHERE id=$1`, id)
		}
	})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	ins := `INSERT INTO events (id, origin_time, location, depth_km, magnitude, mag_type, place_name, version)
	        VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography, $5, $6, $7, $8, $9)`
	exec(ins, big, base, 28.15, 39.21, 10.2, 4.1, "Mw", "Sındırgı (Balıkesir)", 3)
	exec(ins, small, base.Add(-time.Minute), 30.0, 38.0, nil, 1.9, nil, nil, 1)
	exec(ins, old, base.Add(-48*time.Hour), 30.0, 38.0, 5.0, 3.0, "ML", "Eski", 1)

	rep := `INSERT INTO reports (source, source_event_id, origin_time, location, depth_km, magnitude, mag_type, place_name, raw, event_id)
	        VALUES ($1, $2, $3, ST_SetSRID(ST_MakePoint($4, $5), 4326)::geography, $6, $7, $8, $9, '{}', $10)`
	exec(rep, "emsc", "e-"+big, base, 28.12, 39.23, 10.0, 4.2, "M", "WESTERN TURKEY", big)
	exec(rep, "afad", "a-"+big, base, 28.15, 39.21, 10.2, 4.1, "Mw", "Sındırgı (Balıkesir)", big)
	exec(rep, "kandilli", "k-"+big, base, 28.17, 39.19, 7.0, 4.3, "ML", "SINDIRGI", big)

	sum := `INSERT INTO summaries (event_id, event_version, text, generator, validated) VALUES ($1, $2, $3, $4, $5)`
	exec(sum, big, 1, "eski özet", "template", true)
	exec(sum, big, 2, "güncel özet", "llm:x", true)
	exec(sum, big, 3, "doğrulanmamış", "llm:x", false)

	got, err := s.RecentEvents(ctx, base.Add(-time.Hour), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != big || got[1].ID != small {
		t.Fatalf("beklenmeyen olaylar: %+v", got)
	}
	e := got[0]
	if e.Lat != 39.21 || e.Lon != 28.15 || e.Magnitude != 4.1 || *e.DepthKm != 10.2 || e.Version != 3 {
		t.Fatalf("alanlar: %+v", e)
	}
	if !slices.Equal(e.Sources, []string{"afad", "kandilli", "emsc"}) {
		t.Fatalf("kaynaklar: %v", e.Sources)
	}
	if e.Summary == nil || *e.Summary != "güncel özet" {
		t.Fatalf("özet: %v", e.Summary)
	}
	if s2 := got[1]; s2.DepthKm != nil || s2.Summary != nil || len(s2.Sources) != 0 || s2.PlaceName != "" {
		t.Fatalf("boş alanlar: %+v", s2)
	}

	// min_mag ve limit
	got, err = s.RecentEvents(ctx, base.Add(-time.Hour), 2, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("min_mag: %v %+v", err, got)
	}
	got, err = s.RecentEvents(ctx, base.Add(-72*time.Hour), 0, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("limit: %v %+v", err, got)
	}

	d, err := s.EventDetail(ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Reports) != 3 || d.Reports[0].Source != "emsc" || d.Reports[2].Lat != 39.19 {
		t.Fatalf("raporlar: %+v", d.Reports)
	}
	if _, err := s.EventDetail(ctx, newUUID(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bulunamadı beklenirdi: %v", err)
	}
}
