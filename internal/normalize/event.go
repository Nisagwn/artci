package normalize

import "time"

// Event, birleştirilmiş olaydır (kullanıcının gördüğü deprem).
// merger tarafından events.updated akışına bu biçimde yazılır.
type Event struct {
	ID         string    `json:"id"`
	OriginTime time.Time `json:"origin_time"`
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	DepthKm    *float64  `json:"depth_km,omitempty"`
	Magnitude  float64   `json:"magnitude"`
	MagType    string    `json:"mag_type,omitempty"`
	PlaceName  string    `json:"place_name,omitempty"`
	Version    int       `json:"version"`
	Sources    []string  `json:"sources"` // bu olayı raporlayan kaynaklar
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// FirstReportedAt: ilk raporun bize ulaştığı an; uçtan uca gecikme ölçümü için.
	FirstReportedAt time.Time `json:"first_reported_at"`
}

// EventUpdate, events.updated akışındaki mesajdır.
type EventUpdate struct {
	Event Event `json:"event"`
	// PrevMagnitude: önceki sürümün büyüklüğü (yeni olaysa nil).
	// notifier güncelleme kuralı için kullanır (Plan §10).
	PrevMagnitude *float64 `json:"prev_magnitude,omitempty"`
	IsNew         bool     `json:"is_new"`
}
