package store

// merger'ın veritabanı işleri. Tüm yazmalar tek bir pgx.Tx içinde yapılır
// (ReportTx): rapor upsert'i ile olay güncellemesi ya birlikte olur ya hiç.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nisaguven/artci/internal/match"
	"github.com/nisaguven/artci/internal/normalize"
)

// UpsertStatus, UpsertReport'un ne yaptığını söyler.
type UpsertStatus int

const (
	ReportInserted  UpsertStatus = iota // ilk kez görüldü
	ReportUpdated                       // kaynak değerlerini değiştirdi
	ReportUnchanged                     // aynı mesaj tekrar geldi: no-op
)

func (s UpsertStatus) String() string {
	switch s {
	case ReportInserted:
		return "inserted"
	case ReportUpdated:
		return "updated"
	default:
		return "unchanged"
	}
}

// UpsertResult, UpsertReport'un sonucudur.
type UpsertResult struct {
	ReportID int64
	EventID  string // rapor henüz bir olaya bağlı değilse ""
	Status   UpsertStatus
}

// ReportTx, merger'ın tek transaction'lık çalışma birimidir.
type ReportTx struct {
	tx pgx.Tx
}

// InReportTx, fn'i tek bir transaction içinde çalıştırır; fn hata dönerse
// her şey geri alınır (rapor da yazılmamış olur, mesaj yeniden işlenebilir).
func (s *Store) InReportTx(ctx context.Context, fn func(*ReportTx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return fn(&ReportTx{tx: tx})
	})
}

// point: PostGIS'te (lon, lat) sırası! Tersini yazmak en yaygın hatadır.
const point = `ST_SetSRID(ST_MakePoint(%s, %s), 4326)::geography`

func pt(lon, lat string) string { return fmt.Sprintf(point, lon, lat) }

// UpsertReport, raporu (source, source_event_id) anahtarıyla yazar.
//
// Idempotency'nin kalbi: UNIQUE kısıtı + ON CONFLICT. DO UPDATE'in WHERE'i
// yalnızca anlamlı alanlar değiştiyse günceller; aynı mesaj ikinci kez
// gelirse hiçbir satır dönmez ve ReportUnchanged bildirilir. received_at
// bilerek güncellenmez: "bu rapor bize ilk ne zaman ulaştı" bilgisi gecikme
// ölçümünün temelidir.
func (t *ReportTx) UpsertReport(ctx context.Context, r normalize.Report) (UpsertResult, error) {
	raw := []byte(r.Raw)
	if len(raw) == 0 || !json.Valid(raw) {
		raw = []byte("{}") // raw NOT NULL; bozuk ham veri eşleştirmeyi engellememeli
	}
	var received any // sıfırsa DB saatini kullan
	if !r.ReceivedAt.IsZero() {
		received = r.ReceivedAt
	}
	q := `
INSERT INTO reports (source, source_event_id, origin_time, location, depth_km,
                     magnitude, mag_type, place_name, raw, received_at)
VALUES ($1, $2, $3, ` + pt("$4", "$5") + `, $6, $7, NULLIF($8::text, ''), NULLIF($9::text, ''),
        $10, COALESCE($11::timestamptz, now()))
ON CONFLICT (source, source_event_id) DO UPDATE SET
    origin_time = EXCLUDED.origin_time,
    location    = EXCLUDED.location,
    depth_km    = EXCLUDED.depth_km,
    magnitude   = EXCLUDED.magnitude,
    mag_type    = EXCLUDED.mag_type,
    place_name  = EXCLUDED.place_name,
    raw         = EXCLUDED.raw
WHERE reports.origin_time IS DISTINCT FROM EXCLUDED.origin_time
   OR NOT ST_Equals(reports.location::geometry, EXCLUDED.location::geometry)
   OR reports.depth_km   IS DISTINCT FROM EXCLUDED.depth_km
   OR reports.magnitude  IS DISTINCT FROM EXCLUDED.magnitude
   OR reports.mag_type   IS DISTINCT FROM EXCLUDED.mag_type
   OR reports.place_name IS DISTINCT FROM EXCLUDED.place_name
RETURNING id, COALESCE(event_id::text, ''), (xmax = 0) AS inserted`
	// xmax = 0: satır bu komutla yeni eklendi (güncellenen satırda xmax dolu olur).

	var res UpsertResult
	var inserted bool
	err := t.tx.QueryRow(ctx, q,
		r.Source, r.SourceEventID, r.OriginTime, r.Lon, r.Lat, r.DepthKm,
		r.Magnitude, r.MagType, r.PlaceName, raw, received,
	).Scan(&res.ReportID, &res.EventID, &inserted)
	switch {
	case err == nil:
		res.Status = ReportUpdated
		if inserted {
			res.Status = ReportInserted
		}
		return res, nil
	case errors.Is(err, pgx.ErrNoRows):
		// WHERE yanlış -> hiçbir şey değişmedi. ON CONFLICT satırı yine de kilitler.
		res.Status = ReportUnchanged
		err = t.tx.QueryRow(ctx,
			`SELECT id, COALESCE(event_id::text, '') FROM reports WHERE source = $1 AND source_event_id = $2`,
			r.Source, r.SourceEventID).Scan(&res.ReportID, &res.EventID)
		return res, err
	default:
		return res, fmt.Errorf("upsert report: %w", err)
	}
}

// FindCandidateEvents, Plan §8 adım 2: origin_time ±window ve radiusKm içindeki
// olaylar; her biri için bağlı raporların özeti (aynı-kaynak kuralı için).
// Zaman aralığı B-tree, mesafe GIST indeksini kullanır.
func (t *ReportTx) FindCandidateEvents(ctx context.Context, r normalize.Report, window time.Duration, radiusKm float64) ([]match.Candidate, error) {
	q := `
SELECT e.id::text, e.origin_time, ST_Y(e.location::geometry), ST_X(e.location::geometry), e.magnitude::float8,
       r.source, r.source_event_id, r.origin_time,
       ST_Y(r.location::geometry), ST_X(r.location::geometry), r.magnitude::float8
FROM events e
JOIN reports r ON r.event_id = e.id
WHERE e.origin_time BETWEEN $1 AND $2
  AND ST_DWithin(e.location, ` + pt("$3", "$4") + `, $5)
ORDER BY e.id`
	rows, err := t.tx.Query(ctx, q,
		r.OriginTime.Add(-window), r.OriginTime.Add(window), r.Lon, r.Lat, radiusKm*1000)
	if err != nil {
		return nil, fmt.Errorf("find candidates: %w", err)
	}
	defer rows.Close()

	var out []match.Candidate
	for rows.Next() {
		var c match.Candidate
		var m match.Member
		if err := rows.Scan(&c.EventID, &c.OriginTime, &c.Lat, &c.Lon, &c.Magnitude,
			&m.Source, &m.SourceEventID, &m.OriginTime, &m.Lat, &m.Lon, &m.Magnitude); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].EventID == c.EventID {
			out[n-1].Members = append(out[n-1].Members, m)
			continue
		}
		c.Members = []match.Member{m}
		out = append(out, c)
	}
	return out, rows.Err()
}

// eventCols: olay okumalarının ortak sütunları. Kaynak listesi saklanmaz,
// raporlardan türetilir (tek doğruluk kaynağı reports tablosu).
const eventCols = `
e.id::text, e.origin_time, ST_Y(e.location::geometry), ST_X(e.location::geometry),
e.depth_km::float8, e.magnitude::float8, COALESCE(e.mag_type, ''), COALESCE(e.place_name, ''),
e.version, e.created_at, e.updated_at, e.first_reported_at,
(SELECT array_agg(DISTINCT r.source) FROM reports r WHERE r.event_id = e.id)`

// sortSources query.go içinde tanımlı; match.Resolve ile aynı öncelik sırasını
// kullanır, yoksa karşılaştırma her seferinde "değişti" derdi.
func scanMergeEvent(row pgx.Row, extra ...any) (normalize.Event, error) {
	var ev normalize.Event
	dest := []any{&ev.ID, &ev.OriginTime, &ev.Lat, &ev.Lon, &ev.DepthKm, &ev.Magnitude,
		&ev.MagType, &ev.PlaceName, &ev.Version, &ev.CreatedAt, &ev.UpdatedAt,
		&ev.FirstReportedAt, &ev.Sources}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return ev, err
	}
	sortSources(ev.Sources)
	return ev, nil
}

// GetEvent, olayı (kaynak listesiyle) okur.
func (t *ReportTx) GetEvent(ctx context.Context, id string) (normalize.Event, error) {
	ev, err := scanMergeEvent(t.tx.QueryRow(ctx, `SELECT `+eventCols+` FROM events e WHERE e.id = $1::uuid`, id))
	if err != nil {
		return ev, fmt.Errorf("get event %s: %w", id, err)
	}
	return ev, nil
}

// ReportsForEvent, olaya bağlı raporları döner (Resolve girdisi).
func (t *ReportTx) ReportsForEvent(ctx context.Context, eventID string) ([]normalize.Report, error) {
	rows, err := t.tx.Query(ctx, `
SELECT source, source_event_id, origin_time, ST_Y(location::geometry), ST_X(location::geometry),
       depth_km::float8, magnitude::float8, COALESCE(mag_type, ''), COALESCE(place_name, ''), received_at
FROM reports WHERE event_id = $1::uuid`, eventID)
	if err != nil {
		return nil, fmt.Errorf("reports for event: %w", err)
	}
	defer rows.Close()
	var out []normalize.Report
	for rows.Next() {
		var r normalize.Report
		if err := rows.Scan(&r.Source, &r.SourceEventID, &r.OriginTime, &r.Lat, &r.Lon,
			&r.DepthKm, &r.Magnitude, &r.MagType, &r.PlaceName, &r.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateEvent, yeni olayı version=1 ile yazar. ev.ID çağıran tarafından üretilir
// (uuid) ki rapor aynı transaction içinde hemen bağlanabilsin.
func (t *ReportTx) CreateEvent(ctx context.Context, ev normalize.Event) error {
	_, err := t.tx.Exec(ctx, `
INSERT INTO events (id, origin_time, location, depth_km, magnitude, mag_type, place_name, version, first_reported_at)
VALUES ($1::uuid, $2, `+pt("$3", "$4")+`, $5, $6, NULLIF($7::text, ''), NULLIF($8::text, ''), 1, $9)`,
		ev.ID, ev.OriginTime, ev.Lon, ev.Lat, ev.DepthKm, ev.Magnitude, ev.MagType, ev.PlaceName, ev.FirstReportedAt)
	if err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	return nil
}

// UpdateEvent, olayın gösterilen değerlerini yazar ve version'ı bir artırır.
// Yeni sürüm numarasını döner.
func (t *ReportTx) UpdateEvent(ctx context.Context, ev normalize.Event) (int, error) {
	var v int
	err := t.tx.QueryRow(ctx, `
UPDATE events SET origin_time = $2, location = `+pt("$3", "$4")+`, depth_km = $5, magnitude = $6,
       mag_type = NULLIF($7::text, ''), place_name = NULLIF($8::text, ''), first_reported_at = $9,
       version = version + 1, updated_at = now()
WHERE id = $1::uuid
RETURNING version`,
		ev.ID, ev.OriginTime, ev.Lon, ev.Lat, ev.DepthKm, ev.Magnitude, ev.MagType, ev.PlaceName, ev.FirstReportedAt,
	).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("update event %s: %w", ev.ID, err)
	}
	return v, nil
}

// AttachReport, raporu olaya bağlar. eventID "" ise bağı koparır (olay bölünmesi).
func (t *ReportTx) AttachReport(ctx context.Context, reportID int64, eventID string) error {
	_, err := t.tx.Exec(ctx, `UPDATE reports SET event_id = NULLIF($2::text, '')::uuid WHERE id = $1`, reportID, eventID)
	if err != nil {
		return fmt.Errorf("attach report: %w", err)
	}
	return nil
}

// UnpublishedEvents, son yayından beri sürümü artmış olayları events.updated
// mesajı olarak döner (002_merger.sql outbox'ı). PrevMagnitude, tüketicilerin
// en son gördüğü büyüklüktür; IsNew, olay hiç yayınlanmadıysa true.
func (s *Store) UnpublishedEvents(ctx context.Context) ([]normalize.EventUpdate, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+eventCols+`, e.published_version, e.published_magnitude::float8
FROM events e WHERE e.published_version < e.version ORDER BY e.updated_at`)
	if err != nil {
		return nil, fmt.Errorf("unpublished events: %w", err)
	}
	defer rows.Close()
	var out []normalize.EventUpdate
	for rows.Next() {
		var pubVersion int
		var pubMag *float64
		ev, err := scanMergeEvent(rows, &pubVersion, &pubMag)
		if err != nil {
			return nil, err
		}
		out = append(out, normalize.EventUpdate{Event: ev, PrevMagnitude: pubMag, IsNew: pubVersion == 0})
	}
	return out, rows.Err()
}

// MarkPublished, olayın verilen sürümünün yayınlandığını kaydeder. Koşul,
// eski bir sürümün yenisinin üzerine yazılmasını engeller.
func (s *Store) MarkPublished(ctx context.Context, eventID string, version int, magnitude float64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE events SET published_version = $2, published_magnitude = $3
WHERE id = $1::uuid AND published_version < $2`, eventID, version, magnitude)
	if err != nil {
		return fmt.Errorf("mark published: %w", err)
	}
	return nil
}
