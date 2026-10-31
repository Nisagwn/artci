package merger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nisaguven/artci/internal/match"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
	"github.com/nisaguven/artci/migrations"
)

// Gerçek PostGIS ile tam hikâye. ARTCI_TEST_POSTGRES_DSN yoksa atlanır:
//
//	ARTCI_TEST_POSTGRES_DSN=postgres://artci:artci@localhost:5432/artci?sslmode=disable go test ./internal/merger -run Integration
func TestIntegration_FullStory(t *testing.T) {
	dsn := os.Getenv("ARTCI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ARTCI_TEST_POSTGRES_DSN tanımlı değil; entegrasyon testi atlandı")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	migrateLocked(t, st)

	pub := &memPub{}
	svc := New(PGStore{S: st}, pub, match.DefaultConfig())
	svc.Log = slog.New(slog.NewTextHandler(io.Discard, nil))

	// Rastgele geçmiş bir an ve benzersiz id'ler: önceki koşuların ve paralel
	// testlerin verisiyle karışmasın (tablo boşaltılmıyor).
	base := time.Date(2003, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rand.Int64N(1e6)) * time.Minute)
	run := uuid.NewString()[:8]
	mk := func(src string, dt time.Duration, lat, lon, mag float64, magType string) normalize.Report {
		d := 10.0
		return normalize.Report{Source: src, SourceEventID: fmt.Sprintf("%s-%s-%d", run, src, dt/time.Second),
			OriginTime: base.Add(dt), Lat: lat, Lon: lon, DepthKm: &d, Magnitude: mag, MagType: magType,
			Raw: []byte(`{}`), ReceivedAt: base.Add(dt + time.Minute)}
	}
	emsc := mk(normalize.SourceEMSC, time.Second, 39.23, 28.12, 4.2, "M")
	kand := mk(normalize.SourceKandilli, -time.Second, 39.19, 28.17, 4.3, "ML")
	afad := mk(normalize.SourceAFAD, 0, 39.21, 28.15, 4.1, "Mw")

	step := func(r normalize.Report, want Decision) {
		t.Helper()
		got, err := svc.Handle(ctx, r)
		if err != nil {
			t.Fatalf("%s: %v", r.SourceEventID, err)
		}
		if got != want {
			t.Fatalf("%s: karar=%s, beklenen %s", r.SourceEventID, got, want)
		}
	}
	step(emsc, DecisionNewEvent)
	step(kand, DecisionAttached)
	step(afad, DecisionAttached)
	step(afad, DecisionNoop) // aynı mesaj ikinci kez
	kand.Magnitude = 4.1     // Kandilli düzeltir; AFAD öncelikli olduğundan gösterilen değişmez
	step(kand, DecisionUpdate)
	afad.Magnitude = 4.0 // AFAD düzeltir: olay değişir
	step(afad, DecisionUpdate)

	// 20 sn sonra ~15 km ötede gerçek bir artçı: ayrı olay.
	step(mk(normalize.SourceAFAD, 20*time.Second, 39.345, 28.15, 3.8, "Mw"), DecisionNewEvent)

	var eventIDs []string
	if err := st.Pool.QueryRow(ctx, `SELECT array_agg(DISTINCT event_id::text) FROM reports
		WHERE source_event_id LIKE $1 || '-%'`, run).Scan(&eventIDs); err != nil {
		t.Fatal(err)
	}
	if len(eventIDs) != 2 {
		t.Fatalf("2 olay bekleniyordu: %v", eventIDs)
	}

	var n, version int
	var mag float64
	err = st.Pool.QueryRow(ctx, `SELECT count(*), max(e.version), max(e.magnitude)::float8
		FROM reports r JOIN events e ON e.id = r.event_id
		WHERE r.source_event_id LIKE $1 || '-%' AND r.origin_time < $2`, run, base.Add(10*time.Second)).
		Scan(&n, &version, &mag)
	if err != nil {
		t.Fatal(err)
	}
	// v1 EMSC, v2 +Kandilli, v3 +AFAD, v4 AFAD düzeltmesi. Kandilli düzeltmesi sürüm artırmaz.
	if n != 3 || version != 4 || mag != 4.0 {
		t.Fatalf("ana olay: rapor=%d version=%d M=%.1f", n, version, mag)
	}

	// Yayınlar: ana olay 4 sürüm + artçı 1 = 5 (başka testlerin olayları süzülür).
	mine := map[string]bool{eventIDs[0]: true, eventIDs[1]: true}
	var ours []normalize.EventUpdate
	for _, u := range pub.got {
		if mine[u.Event.ID] {
			ours = append(ours, u)
		}
	}
	if len(ours) != 5 {
		t.Fatalf("5 yayın bekleniyordu: %d", len(ours))
	}
	var pending int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE id = ANY($1::text[]::uuid[])
		AND published_version < version`, eventIDs).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("yayınlanmamış sürüm kalmamalı: %d %v", pending, err)
	}
}

func migrateLocked(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := st.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// go test paketleri paralel koşar; store testleriyle aynı anda migration olmasın.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(424242)`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock(424242)`)
	if err := st.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
}
