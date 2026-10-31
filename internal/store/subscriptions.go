package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nisaguven/artci/internal/normalize"
)

// Subscription, bir Telegram sohbetinin bir il için aboneliğidir.
// Konum olarak kişinin değil, il merkezinin koordinatı saklanır (Plan §17).
type Subscription struct {
	ID        int64
	ChatID    int64
	City      string
	Lat, Lon  float64
	RadiusKm  int
	MinMag    float64
	CreatedAt time.Time
}

// Bildirim türleri (notifications.kind).
const (
	KindInitial = "initial"
	KindUpdate  = "update"
)

const subCols = `id, chat_id, city, ST_Y(center::geometry), ST_X(center::geometry), radius_km, min_mag::float8, created_at`

func scanSubs(rows pgx.Rows) ([]Subscription, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subscription, error) {
		var s Subscription
		err := r.Scan(&s.ID, &s.ChatID, &s.City, &s.Lat, &s.Lon, &s.RadiusKm, &s.MinMag, &s.CreatedAt)
		return s, err
	})
}

// roundMag1: NUMERIC(3,1) sütunlarına float yazarken ikili kayan nokta artıklarını
// (4.1 -> 4.0999...) temizler.
func roundMag1(v float64) float64 { return math.Round(v*10) / 10 }

// AddSubscription, aboneliği ekler. Aynı sohbet aynı il için zaten aboneyse
// yeni satır açmak yerine yarıçap ve eşiği günceller (updated=true); böylece
// kullanıcı ayarını değiştirmek için önce /iptal yazmak zorunda kalmaz.
func (s *Store) AddSubscription(ctx context.Context, sub Subscription) (out Subscription, updated bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `UPDATE subscriptions
		SET radius_km = $3, min_mag = $4,
		    center = ST_SetSRID(ST_MakePoint($6, $5), 4326)::geography
		WHERE chat_id = $1 AND city = $2
		RETURNING `+subCols,
		sub.ChatID, sub.City, sub.RadiusKm, roundMag1(sub.MinMag), sub.Lat, sub.Lon)
	if err != nil {
		return out, false, err
	}
	got, err := scanSubs(rows)
	if err != nil {
		return out, false, err
	}
	if len(got) > 0 {
		out, updated = got[0], true
	} else {
		rows, err = tx.Query(ctx, `INSERT INTO subscriptions (chat_id, city, center, radius_km, min_mag)
			VALUES ($1, $2, ST_SetSRID(ST_MakePoint($6, $5), 4326)::geography, $3, $4)
			RETURNING `+subCols,
			sub.ChatID, sub.City, sub.RadiusKm, roundMag1(sub.MinMag), sub.Lat, sub.Lon)
		if err != nil {
			return out, false, err
		}
		got, err = scanSubs(rows)
		if err != nil {
			return out, false, err
		}
		if len(got) != 1 {
			return out, false, errors.New("abonelik eklenemedi")
		}
		out = got[0]
	}
	return out, updated, tx.Commit(ctx)
}

// ListSubscriptions, sohbetin aboneliklerini eklenme sırasıyla döner.
func (s *Store) ListSubscriptions(ctx context.Context, chatID int64) ([]Subscription, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+subCols+` FROM subscriptions WHERE chat_id = $1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	return scanSubs(rows)
}

// DeleteSubscriptions, sohbetin tüm aboneliklerini siler (/iptal, Plan §17).
// notifications satırları ON DELETE CASCADE ile birlikte silinir; sohbet
// kimliği veritabanında hiçbir yerde kalmaz.
func (s *Store) DeleteSubscriptions(ctx context.Context, chatID int64) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM subscriptions WHERE chat_id = $1`, chatID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AffectedSubscriptions, olaydan etkilenen abonelikleri bulur: olay noktası
// aboneliğin yarıçapı içinde ve büyüklük aboneliğin eşiğinde/üstünde.
// ST_DWithin geography üzerinde metre ile çalışır ve GIST indeksini kullanır.
func (s *Store) AffectedSubscriptions(ctx context.Context, ev normalize.Event) ([]Subscription, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+subCols+` FROM subscriptions
		WHERE ST_DWithin(center, ST_SetSRID(ST_MakePoint($2, $1), 4326)::geography, radius_km * 1000.0)
		  AND min_mag <= $3::numeric
		ORDER BY id`,
		ev.Lat, ev.Lon, roundMag1(ev.Magnitude))
	if err != nil {
		return nil, fmt.Errorf("affected subscriptions: %w", err)
	}
	return scanSubs(rows)
}

// RecordNotification, bildirim günlüğüne satır ekler. Satır eklendiyse true döner
// ve çağıran mesajı gönderir; zaten varsa false döner ve gönderim yapılmaz.
// Exactly-once *etkisi* buradan gelir: tüketici at-least-once çalışsa da
// (aynı olay mesajı iki kez işlense de) PRIMARY KEY ikinci gönderimi engeller.
func (s *Store) RecordNotification(ctx context.Context, eventID string, subID int64, kind string, mag float64) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO notifications (event_id, subscription_id, kind, magnitude)
		VALUES ($1::uuid, $2, $3, $4) ON CONFLICT DO NOTHING`,
		eventID, subID, kind, roundMag1(mag))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteNotification, gönderimi başarısız olan bildirimin kaydını geri alır ki
// olay mesajı yeniden işlendiğinde tekrar denensin.
func (s *Store) DeleteNotification(ctx context.Context, eventID string, subID int64, kind string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM notifications WHERE event_id = $1::uuid AND subscription_id = $2 AND kind = $3`,
		eventID, subID, kind)
	return err
}

// NotifyState, bir abonenin bir olay için şimdiye kadar aldığı bildirimlerdir.
type NotifyState struct {
	HasInitial bool
	HasUpdate  bool
	// LastMag: en son bildirilen büyüklük (update varsa onun, yoksa initial'ın).
	LastMag float64
}

// LastNotifiedMagnitude, abonenin bu olay için aldığı bildirimleri ve en son
// bildirilen büyüklüğü döner. Güncelleme kararı olayın önceki sürümüne değil,
// kullanıcıya gerçekten söylenmiş değere göre verilir.
func (s *Store) LastNotifiedMagnitude(ctx context.Context, eventID string, subID int64) (NotifyState, error) {
	rows, err := s.Pool.Query(ctx, `SELECT kind, COALESCE(magnitude, 0)::float8 FROM notifications
		WHERE event_id = $1::uuid AND subscription_id = $2`, eventID, subID)
	if err != nil {
		return NotifyState{}, err
	}
	defer rows.Close()
	var st NotifyState
	var initMag float64
	for rows.Next() {
		var kind string
		var mag float64
		if err := rows.Scan(&kind, &mag); err != nil {
			return NotifyState{}, err
		}
		switch kind {
		case KindInitial:
			st.HasInitial, initMag = true, mag
		case KindUpdate:
			st.HasUpdate, st.LastMag = true, mag
		}
	}
	if !st.HasUpdate {
		st.LastMag = initMag
	}
	return st, rows.Err()
}
