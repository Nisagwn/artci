package merger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/nisaguven/artci/internal/geo"
	"github.com/nisaguven/artci/internal/match"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
)

// memStore, store.ReportTx'in bellek içi taklididir. Amaç merger mantığını
// Docker'sız, uçtan uca ve hızlı test etmek. PostgreSQL'in davranışını
// gereken yerde taklit eder: NUMERIC(…,1) yuvarlama, mikro saniye, received_at'in
// çakışmada korunması, transaction geri alma.
type memStore struct {
	mu      sync.Mutex
	reports map[string]*memReport // anahtar: source|source_event_id
	events  map[string]*memEvent
	nextID  int64
	now     time.Time

	// Dizinler: gerçek dizide (binlerce rapor) her çağrıda tüm raporları
	// taramak taramayı saatlere çıkarıyordu.
	byID    map[int64]*memReport
	byEvent map[string]map[*memReport]bool

	undo []func() // açık transaction'ın geri alma günlüğü (ters sırayla çalışır)

	failTx error // != nil ise InTx fn'den sonra bu hatayla geri alır (çökme taklidi)
}

type memReport struct {
	ID      int64
	R       normalize.Report
	EventID string
}

type memEvent struct {
	Ev         normalize.Event // Sources tutulmaz; raporlardan türetilir
	PubVersion int
	PubMag     *float64
}

func newMemStore() *memStore {
	return &memStore{reports: map[string]*memReport{}, events: map[string]*memEvent{},
		byID: map[int64]*memReport{}, byEvent: map[string]map[*memReport]bool{},
		now: time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)}
}

func key(src, id string) string { return src + "|" + id }

func r1(x float64) float64 { return math.Round(x*10) / 10 }

// dbify, değerleri PostgreSQL'in saklayacağı hassasiyete indirger.
func dbify(r normalize.Report) normalize.Report {
	r.OriginTime = r.OriginTime.UTC().Round(time.Microsecond)
	r.ReceivedAt = r.ReceivedAt.UTC().Round(time.Microsecond)
	r.Magnitude = r1(r.Magnitude)
	if r.DepthKm != nil {
		d := r1(*r.DepthKm)
		r.DepthKm = &d
	}
	r.Raw = nil
	return r
}

// setEventID, raporun olayını değiştirir ve dizini günceller.
func (m *memStore) setEventID(r *memReport, eventID string) {
	if r.EventID != "" {
		delete(m.byEvent[r.EventID], r)
	}
	r.EventID = eventID
	if eventID != "" {
		if m.byEvent[eventID] == nil {
			m.byEvent[eventID] = map[*memReport]bool{}
		}
		m.byEvent[eventID][r] = true
	}
}

// eventReports, olaya bağlı raporlardır (sırasız; map gibi).
func (m *memStore) eventReports(eventID string) map[*memReport]bool { return m.byEvent[eventID] }

func (m *memStore) InTx(ctx context.Context, fn func(Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.undo = m.undo[:0]
	err := fn(&memTx{m})
	if err == nil && m.failTx != nil {
		err = m.failTx
	}
	if err != nil { // ROLLBACK
		for i := len(m.undo) - 1; i >= 0; i-- {
			m.undo[i]()
		}
	}
	m.undo = m.undo[:0]
	return err
}

func (m *memStore) sources(eventID string) []string {
	seen := map[string]bool{}
	var out []string
	for r := range m.eventReports(eventID) {
		if !seen[r.R.Source] {
			seen[r.R.Source] = true
			out = append(out, r.R.Source)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return normalize.SourcePriority[out[i]] < normalize.SourcePriority[out[j]]
	})
	return out
}

func (m *memStore) event(id string) normalize.Event {
	ev := m.events[id].Ev
	ev.Sources = m.sources(id)
	return ev
}

func (m *memStore) UnpublishedEvents(ctx context.Context) ([]normalize.EventUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []normalize.EventUpdate
	for id, e := range m.events {
		if e.PubVersion < e.Ev.Version {
			out = append(out, normalize.EventUpdate{Event: m.event(id), PrevMagnitude: e.PubMag, IsNew: e.PubVersion == 0})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Event.ID < out[j].Event.ID })
	return out, nil
}

func (m *memStore) MarkPublished(ctx context.Context, id string, version int, mag float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.events[id]; e != nil && e.PubVersion < version {
		e.PubVersion = version
		v := r1(mag)
		e.PubMag = &v
	}
	return nil
}

// memTx, InTx içinde kilit tutulurken çağrılır.
type memTx struct{ m *memStore }

func (t *memTx) UpsertReport(ctx context.Context, r normalize.Report) (store.UpsertResult, error) {
	m := t.m
	r = dbify(r)
	if r.ReceivedAt.IsZero() {
		r.ReceivedAt = m.now
	}
	k := key(r.Source, r.SourceEventID)
	if old, ok := m.reports[k]; ok {
		res := store.UpsertResult{ReportID: old.ID, EventID: old.EventID, Status: store.ReportUnchanged}
		if match.ReportChanged(old.R, r) {
			r.ReceivedAt = old.R.ReceivedAt // SQL'deki gibi received_at korunur
			prev := old.R
			m.undo = append(m.undo, func() { old.R = prev })
			old.R = r
			res.Status = store.ReportUpdated
		}
		return res, nil
	}
	m.nextID++
	nr := &memReport{ID: m.nextID, R: r}
	m.reports[k], m.byID[nr.ID] = nr, nr
	m.undo = append(m.undo, func() { delete(m.reports, k); delete(m.byID, nr.ID); m.nextID-- })
	return store.UpsertResult{ReportID: m.nextID, Status: store.ReportInserted}, nil
}

func (t *memTx) FindCandidateEvents(ctx context.Context, r normalize.Report, window time.Duration, radiusKm float64) ([]match.Candidate, error) {
	m := t.m
	var out []match.Candidate
	for id, e := range m.events {
		dt := e.Ev.OriginTime.Sub(r.OriginTime)
		if dt < -window || dt > window || geo.HaversineKm(e.Ev.Lat, e.Ev.Lon, r.Lat, r.Lon) > radiusKm {
			continue
		}
		c := match.Candidate{EventID: id, OriginTime: e.Ev.OriginTime, Lat: e.Ev.Lat, Lon: e.Ev.Lon, Magnitude: e.Ev.Magnitude}
		for rep := range m.eventReports(id) {
			c.Members = append(c.Members, match.MemberOf(rep.R))
		}
		out = append(out, c)
	}
	// Map sırası rastgele: Decide'ın sıradan bağımsız olduğunu bu da sınar.
	return out, nil
}

func (t *memTx) GetEvent(ctx context.Context, id string) (normalize.Event, error) {
	if _, ok := t.m.events[id]; !ok {
		return normalize.Event{}, fmt.Errorf("olay yok: %s", id)
	}
	return t.m.event(id), nil
}

func (t *memTx) ReportsForEvent(ctx context.Context, eventID string) ([]normalize.Report, error) {
	var out []normalize.Report
	for r := range t.m.eventReports(eventID) {
		out = append(out, r.R)
	}
	return out, nil
}

func (t *memTx) CreateEvent(ctx context.Context, ev normalize.Event) error {
	if _, ok := t.m.events[ev.ID]; ok {
		return errors.New("olay zaten var")
	}
	ev.Sources = nil
	ev.Version = 1
	ev.Magnitude = r1(ev.Magnitude)
	ev.CreatedAt, ev.UpdatedAt = t.m.now, t.m.now
	t.m.events[ev.ID] = &memEvent{Ev: ev}
	t.m.undo = append(t.m.undo, func() { delete(t.m.events, ev.ID) })
	return nil
}

func (t *memTx) UpdateEvent(ctx context.Context, ev normalize.Event) (int, error) {
	e, ok := t.m.events[ev.ID]
	if !ok {
		return 0, fmt.Errorf("olay yok: %s", ev.ID)
	}
	ev.Sources = nil
	ev.Version = e.Ev.Version + 1
	ev.Magnitude = r1(ev.Magnitude)
	ev.CreatedAt, ev.UpdatedAt = e.Ev.CreatedAt, t.m.now
	prev := e.Ev
	t.m.undo = append(t.m.undo, func() { e.Ev = prev })
	e.Ev = ev
	return ev.Version, nil
}

func (t *memTx) AttachReport(ctx context.Context, reportID int64, eventID string) error {
	r, ok := t.m.byID[reportID]
	if !ok {
		return fmt.Errorf("rapor yok: %d", reportID)
	}
	if eventID != "" {
		if _, ok := t.m.events[eventID]; !ok {
			return fmt.Errorf("FK ihlali: olay %s yok", eventID)
		}
	}
	prev := r.EventID
	t.m.undo = append(t.m.undo, func() { t.m.setEventID(r, prev) })
	t.m.setEventID(r, eventID)
	return nil
}

// memPub, yayınlanan mesajları biriktirir.
type memPub struct {
	got  []normalize.EventUpdate
	fail error
}

func (p *memPub) Publish(ctx context.Context, u normalize.EventUpdate) error {
	if p.fail != nil {
		return p.fail
	}
	p.got = append(p.got, u)
	return nil
}
