package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nisaguven/artci/internal/normalize"
)

// ErrNotFound, istenen olay yoksa döner.
var ErrNotFound = errors.New("bulunamadı")

// EventView, api'nin gösterdiği olaydır: olay + raporlayan kaynaklar + en güncel özet.
type EventView struct {
	ID         string    `json:"id"`
	OriginTime time.Time `json:"origin_time"`
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	DepthKm    *float64  `json:"depth_km,omitempty"`
	Magnitude  float64   `json:"magnitude"`
	MagType    string    `json:"mag_type,omitempty"`
	PlaceName  string    `json:"place_name,omitempty"`
	Version    int       `json:"version"`
	Sources    []string  `json:"sources"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Summary: doğrulanmış en güncel özet; henüz yoksa nil.
	Summary *string `json:"summary,omitempty"`
}

// ReportView, bir kaynağın bu olay için yayınladığı rapordur.
type ReportView struct {
	Source        string    `json:"source"`
	SourceEventID string    `json:"source_event_id"`
	OriginTime    time.Time `json:"origin_time"`
	Lat           float64   `json:"lat"`
	Lon           float64   `json:"lon"`
	DepthKm       *float64  `json:"depth_km,omitempty"`
	Magnitude     float64   `json:"magnitude"`
	MagType       string    `json:"mag_type,omitempty"`
	PlaceName     string    `json:"place_name,omitempty"`
	ReceivedAt    time.Time `json:"received_at"`
}

// EventDetailView, olay + tüm raporlarıdır.
type EventDetailView struct {
	EventView
	Reports []ReportView `json:"reports"`
}

// Ortak SELECT gövdesi. NUMERIC sütunlar float8'e çevrilir; pgx'in Numeric
// dönüşümüyle uğraşmamak ve JSON'da sayı olarak göndermek için.
// Özet yalnızca validated=true olanlardan seçilir: doğrulanmamış LLM metni
// haritada asla görünmemeli (Plan §11).
const eventSelect = `
SELECT e.id::text, e.origin_time,
       ST_Y(e.location::geometry), ST_X(e.location::geometry),
       e.depth_km::float8, e.magnitude::float8,
       COALESCE(e.mag_type, ''), COALESCE(e.place_name, ''),
       e.version, e.updated_at,
       COALESCE((SELECT array_agg(DISTINCT r.source) FROM reports r WHERE r.event_id = e.id), '{}'::text[]),
       (SELECT s.text FROM summaries s
         WHERE s.event_id = e.id AND s.validated
         ORDER BY s.event_version DESC LIMIT 1)
FROM events e`

func scanEvent(row pgx.Row) (EventView, error) {
	var v EventView
	err := row.Scan(&v.ID, &v.OriginTime, &v.Lat, &v.Lon, &v.DepthKm, &v.Magnitude,
		&v.MagType, &v.PlaceName, &v.Version, &v.UpdatedAt, &v.Sources, &v.Summary)
	sortSources(v.Sources)
	return v, err
}

// sortSources, kaynakları öncelik sırasına dizer (AFAD > Kandilli > EMSC);
// arayüzde hep aynı sırada görünsünler diye.
func sortSources(s []string) {
	rank := func(x string) int {
		if p, ok := normalize.SourcePriority[x]; ok {
			return p
		}
		return len(normalize.SourcePriority)
	}
	sort.SliceStable(s, func(i, j int) bool {
		if rank(s[i]) != rank(s[j]) {
			return rank(s[i]) < rank(s[j])
		}
		return s[i] < s[j]
	})
}

// RecentEvents, since sonrasındaki ve büyüklüğü minMag ve üzeri olayları
// en yeniden eskiye döner.
func (s *Store) RecentEvents(ctx context.Context, since time.Time, minMag float64, limit int) ([]EventView, error) {
	rows, err := s.Pool.Query(ctx, eventSelect+`
WHERE e.origin_time >= $1 AND e.magnitude >= $2
ORDER BY e.origin_time DESC
LIMIT $3`, since, minMag, limit)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}
	defer rows.Close()
	out := []EventView{}
	for rows.Next() {
		v, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// EventDetail, olayı tüm raporlarıyla döner; olay yoksa ErrNotFound.
// id'nin geçerli bir UUID olduğunu çağıran doğrulamalıdır.
func (s *Store) EventDetail(ctx context.Context, id string) (*EventDetailView, error) {
	ev, err := scanEvent(s.Pool.QueryRow(ctx, eventSelect+` WHERE e.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("event detail: %w", err)
	}
	rows, err := s.Pool.Query(ctx, `
SELECT source, source_event_id, origin_time,
       ST_Y(location::geometry), ST_X(location::geometry),
       depth_km::float8, magnitude::float8,
       COALESCE(mag_type, ''), COALESCE(place_name, ''), received_at
FROM reports WHERE event_id = $1::uuid
ORDER BY received_at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("event reports: %w", err)
	}
	defer rows.Close()
	d := &EventDetailView{EventView: ev, Reports: []ReportView{}}
	for rows.Next() {
		var r ReportView
		if err := rows.Scan(&r.Source, &r.SourceEventID, &r.OriginTime, &r.Lat, &r.Lon,
			&r.DepthKm, &r.Magnitude, &r.MagType, &r.PlaceName, &r.ReceivedAt); err != nil {
			return nil, fmt.Errorf("scan report: %w", err)
		}
		d.Reports = append(d.Reports, r)
	}
	return d, rows.Err()
}

// Ping, sağlık kontrolü için veritabanına erişimi doğrular.
func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }
