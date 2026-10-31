package store

import (
	"context"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/migrations"
)

// subTestStore, gerçek PostGIS'e bağlanır; DSN yoksa test atlanır (CI'da isteğe bağlı).
func subTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("ARTCI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ARTCI_TEST_POSTGRES_DSN yok; PostGIS entegrasyon testi atlandı")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return s
}

// subTestEvent, events tablosuna bir olay ekler (notifications FK'si için gerekli).
func subTestEvent(t *testing.T, s *Store, lat, lon, mag float64) normalize.Event {
	t.Helper()
	ctx := context.Background()
	ev := normalize.Event{Lat: lat, Lon: lon, Magnitude: mag, OriginTime: time.Now().UTC()}
	err := s.Pool.QueryRow(ctx, `INSERT INTO events (id, origin_time, location, magnitude)
		VALUES (gen_random_uuid(), $1, ST_SetSRID(ST_MakePoint($3, $2), 4326)::geography, $4)
		RETURNING id::text`, ev.OriginTime, lat, lon, mag).Scan(&ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1::uuid`, ev.ID) })
	return ev
}

func TestSubscriptionsIntegration(t *testing.T) {
	s := subTestStore(t)
	ctx := context.Background()
	// Diğer testlerle çakışmasın diye rastgele, negatif (grup benzeri) sohbet kimlikleri.
	chatA := -rand.Int64N(1 << 40)
	chatB := chatA - 1
	t.Cleanup(func() {
		s.DeleteSubscriptions(context.Background(), chatA)
		s.DeleteSubscriptions(context.Background(), chatB)
	})

	balikesir := Subscription{ChatID: chatA, City: "Balıkesir", Lat: 39.6484, Lon: 27.8826, RadiusKm: 100, MinMag: 3.5}
	subA, updated, err := s.AddSubscription(ctx, balikesir)
	if err != nil || updated || subA.ID == 0 {
		t.Fatalf("add: %+v updated=%v err=%v", subA, updated, err)
	}
	if subA.Lat < 39.64 || subA.Lat > 39.65 || subA.Lon < 27.88 || subA.Lon > 27.89 {
		t.Fatalf("koordinat ters yazılmış olabilir: %+v", subA)
	}
	// Aynı il tekrar: yeni satır değil güncelleme.
	balikesir.MinMag = 3.1
	again, updated, err := s.AddSubscription(ctx, balikesir)
	if err != nil || !updated || again.ID != subA.ID || again.MinMag != 3.1 {
		t.Fatalf("update: %+v updated=%v err=%v", again, updated, err)
	}
	// Uzak bir abonelik (Van) ve yüksek eşikli bir abonelik.
	if _, _, err := s.AddSubscription(ctx, Subscription{ChatID: chatA, City: "Van", Lat: 38.4891, Lon: 43.4089, RadiusKm: 200, MinMag: 3.5}); err != nil {
		t.Fatal(err)
	}
	subHigh, _, err := s.AddSubscription(ctx, Subscription{ChatID: chatB, City: "Balıkesir", Lat: 39.6484, Lon: 27.8826, RadiusKm: 100, MinMag: 4.5})
	if err != nil {
		t.Fatal(err)
	}

	list, err := s.ListSubscriptions(ctx, chatA)
	if err != nil || len(list) != 2 || list[0].City != "Balıkesir" || list[1].City != "Van" {
		t.Fatalf("list: %+v err=%v", list, err)
	}

	// Sındırgı M4.1: yalnızca chatA/Balıkesir etkilenmeli.
	ev := subTestEvent(t, s, 39.24, 28.17, 4.1)
	aff, err := s.AffectedSubscriptions(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int64]bool{}
	for _, a := range aff {
		found[a.ID] = true
	}
	if !found[subA.ID] || found[subHigh.ID] {
		t.Fatalf("affected yanlış: %+v", aff)
	}
	for _, a := range aff {
		if a.ChatID == chatA && a.City == "Van" {
			t.Fatalf("Van etkilenmemeli")
		}
	}
	// Eşik sınırı: M4.5 olunca yüksek eşikli abonelik de dahil (min_mag <= mag).
	ev45 := ev
	ev45.Magnitude = 4.5
	aff, _ = s.AffectedSubscriptions(ctx, ev45)
	found = map[int64]bool{}
	for _, a := range aff {
		found[a.ID] = true
	}
	if !found[subHigh.ID] {
		t.Fatalf("eşitlikte dahil olmalı: %+v", aff)
	}

	// Bildirim günlüğü: ilk kayıt true, ikincisi false (exactly-once etkisi).
	ok, err := s.RecordNotification(ctx, ev.ID, subA.ID, KindInitial, 4.1)
	if err != nil || !ok {
		t.Fatalf("ilk kayıt: %v %v", ok, err)
	}
	ok, err = s.RecordNotification(ctx, ev.ID, subA.ID, KindInitial, 4.1)
	if err != nil || ok {
		t.Fatalf("ikinci kayıt eklenmemeli: %v %v", ok, err)
	}
	st, err := s.LastNotifiedMagnitude(ctx, ev.ID, subA.ID)
	if err != nil || !st.HasInitial || st.HasUpdate || st.LastMag != 4.1 {
		t.Fatalf("state: %+v %v", st, err)
	}
	if ok, _ := s.RecordNotification(ctx, ev.ID, subA.ID, KindUpdate, 4.6); !ok {
		t.Fatal("update kaydı eklenmeli")
	}
	if st, _ := s.LastNotifiedMagnitude(ctx, ev.ID, subA.ID); !st.HasUpdate || st.LastMag != 4.6 {
		t.Fatalf("state: %+v", st)
	}
	// Gönderim başarısızsa geri alma: kayıt silinir, yeniden eklenebilir.
	if err := s.DeleteNotification(ctx, ev.ID, subA.ID, KindUpdate); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.RecordNotification(ctx, ev.ID, subA.ID, KindUpdate, 4.6); !ok {
		t.Fatal("silinen kayıt yeniden eklenebilmeli")
	}

	// /iptal: abonelikler ve bildirim günlüğü birlikte silinir.
	n, err := s.DeleteSubscriptions(ctx, chatA)
	if err != nil || n != 2 {
		t.Fatalf("delete: %d %v", n, err)
	}
	var left int
	s.Pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE subscription_id = $1`, subA.ID).Scan(&left)
	if left != 0 {
		t.Fatalf("bildirim kayıtları kalmamalı: %d", left)
	}
	if list, _ := s.ListSubscriptions(ctx, chatA); len(list) != 0 {
		t.Fatalf("liste boş olmalı: %+v", list)
	}
}
