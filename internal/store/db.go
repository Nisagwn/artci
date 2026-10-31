// Package store, PostgreSQL + PostGIS erişimidir.
//
// Dosya düzeni (her servis kendi dosyasına sahiptir):
//
//	db.go            bağlantı + migration
//	reports.go       merger: rapor upsert, olay okuma/yazma
//	subscriptions.go notifier: abonelik ve bildirim günlüğü
//	summaries.go     summarizer: özetler ve LLM harcaması
//	query.go         api: okuma sorguları
package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store, tüm sorguların alıcısıdır.
type Store struct {
	Pool *pgxpool.Pool
}

// Open, havuzu açar ve bağlantıyı doğrular.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Close havuzu kapatır.
func (s *Store) Close() { s.Pool.Close() }

// migrateLockKey: tüm servislerin paylaştığı advisory lock anahtarı.
const migrateLockKey = 74_2026_31

// Migrate, verilen dosya sistemindeki *.sql dosyalarını ad sırasıyla uygular.
// Uygulananlar schema_migrations tablosunda tutulur; her dosya bir kez çalışır.
// Birden fazla servis aynı anda açılabildiği için işlem pg_advisory_lock ile
// sıraya sokulur; kilit oturuma bağlı olduğundan tek bir bağlantı kullanılır.
func (s *Store) Migrate(ctx context.Context, fsys fs.FS) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("migration kilidi: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, name); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
