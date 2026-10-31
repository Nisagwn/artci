package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/migrations"
)

// openTestStore, gerçek PostGIS ister; ARTCI_TEST_POSTGRES_DSN yoksa testi atlar.
// Test verisi rastgele id ve zamanlarla yazılır; tablo boşaltılmaz ki paralel
// koşan diğer paketlerin testleriyle çakışmasın.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("ARTCI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ARTCI_TEST_POSTGRES_DSN tanımlı değil; entegrasyon testi atlandı")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	// go test paketleri paralel koşar; iki paket aynı anda migration uygulamasın.
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(424242)`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock(424242)`)
	if err := s.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return s
}

func testReport(id string) normalize.Report {
	d := 10.0
	// Rastgele geçmiş bir an: başka testlerin olaylarıyla aday çakışması olmasın.
	at := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rand.Int64N(1e6)) * time.Minute)
	return normalize.Report{
		Source: normalize.SourceKandilli, SourceEventID: id, OriginTime: at,
		Lat: 39.19, Lon: 28.17, DepthKm: &d, Magnitude: 4.3, MagType: "ML",
		PlaceName: "SINDIRGI (BALIKESIR)", Raw: []byte(`{"satir":"..."}`), ReceivedAt: at.Add(40 * time.Second),
	}
}

func TestUpsertReport_Idempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := testReport(fmt.Sprintf("it-%s", uuid.NewString()))

	upsert := func(r normalize.Report) UpsertResult {
		t.Helper()
		var res UpsertResult
		err := s.InReportTx(ctx, func(tx *ReportTx) error {
			var err error
			res, err = tx.UpsertReport(ctx, r)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	first := upsert(r)
	if first.Status != ReportInserted || first.EventID != "" {
		t.Fatalf("ilk yazım: %+v", first)
	}
	again := r
	again.ReceivedAt = r.ReceivedAt.Add(time.Hour) // polling'in yeniden getirmesi
	again.Raw = []byte(`{"satir":"ayni ama yeniden cekildi"}`)
	if res := upsert(again); res.Status != ReportUnchanged || res.ReportID != first.ReportID {
		t.Fatalf("aynı rapor no-op olmalı: %+v", res)
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM reports WHERE source=$1 AND source_event_id=$2`,
		r.Source, r.SourceEventID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tek satır bekleniyordu: n=%d err=%v", n, err)
	}

	upd := r
	upd.Magnitude = 4.1
	if res := upsert(upd); res.Status != ReportUpdated || res.ReportID != first.ReportID {
		t.Fatalf("büyüklük değişimi güncelleme olmalı: %+v", res)
	}
}

func TestEventRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := testReport(fmt.Sprintf("it-%s", uuid.NewString()))

	err := s.InReportTx(ctx, func(tx *ReportTx) error {
		res, err := tx.UpsertReport(ctx, r)
		if err != nil {
			return err
		}
		ev := normalize.Event{ID: uuid.NewString(), OriginTime: r.OriginTime, Lat: r.Lat, Lon: r.Lon,
			DepthKm: r.DepthKm, Magnitude: r.Magnitude, MagType: r.MagType, PlaceName: r.PlaceName,
			FirstReportedAt: r.ReceivedAt}
		if err := tx.CreateEvent(ctx, ev); err != nil {
			return err
		}
		if err := tx.AttachReport(ctx, res.ReportID, ev.ID); err != nil {
			return err
		}

		// Aday araması: 30 sn ve ~2 km öteden bakınca olay bulunmalı.
		probe := r
		probe.Source, probe.SourceEventID = normalize.SourceEMSC, "probe"
		probe.OriginTime = r.OriginTime.Add(30 * time.Second)
		probe.Lat += 0.02
		cands, err := tx.FindCandidateEvents(ctx, probe, time.Minute, 100)
		if err != nil {
			return err
		}
		if len(cands) != 1 || cands[0].EventID != ev.ID || len(cands[0].Members) != 1 ||
			cands[0].Members[0].Source != normalize.SourceKandilli {
			return fmt.Errorf("aday bulunamadı: %+v", cands)
		}
		probe.OriginTime = r.OriginTime.Add(2 * time.Minute)
		if cands, _ := tx.FindCandidateEvents(ctx, probe, time.Minute, 100); len(cands) != 0 {
			return fmt.Errorf("pencere dışı olay aday olmamalı: %+v", cands)
		}

		got, err := tx.GetEvent(ctx, ev.ID)
		if err != nil {
			return err
		}
		if got.Version != 1 || got.Magnitude != 4.3 || got.Lat != r.Lat || got.Lon != r.Lon ||
			len(got.Sources) != 1 || got.Sources[0] != normalize.SourceKandilli {
			return fmt.Errorf("olay gidiş-dönüşü bozuk: %+v", got)
		}
		ev.Magnitude = 4.1
		v, err := tx.UpdateEvent(ctx, ev)
		if err != nil || v != 2 {
			return fmt.Errorf("update: v=%d err=%v", v, err)
		}
		reps, err := tx.ReportsForEvent(ctx, ev.ID)
		if err != nil || len(reps) != 1 || reps[0].SourceEventID != r.SourceEventID {
			return fmt.Errorf("reports for event: %+v %v", reps, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
