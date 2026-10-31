// Package app, tüm servislerin ortak başlangıç kodudur: yapılandırma,
// loglama, sinyal yönetimi ve bağlantılar.
package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

// Env, ortam değişkenini okur; yoksa varsayılanı döner.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// EnvInt, tamsayı ortam değişkenini okur.
func EnvInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// EnvFloat, ondalıklı ortam değişkenini okur.
func EnvFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

// EnvDuration, süre ortam değişkenini okur (ör. "30s").
func EnvDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// Config, servislerin ortak ayarlarıdır.
type Config struct {
	PostgresDSN string
	RedisAddr   string
	MetricsAddr string
}

// LoadConfig, ortak ayarları ortamdan okur.
func LoadConfig(defaultMetricsAddr string) Config {
	return Config{
		PostgresDSN: Env("ARTCI_POSTGRES_DSN", "postgres://artci:artci@localhost:5432/artci?sslmode=disable"),
		RedisAddr:   Env("ARTCI_REDIS_ADDR", "localhost:6379"),
		MetricsAddr: Env("ARTCI_METRICS_ADDR", defaultMetricsAddr),
	}
}

// Init, JSON loglamayı kurar ve SIGINT/SIGTERM ile iptal edilen bir bağlam döner.
func Init(service string) (context.Context, context.CancelFunc) {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(h).With("service", service))
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Redis, istemciyi açar ve bağlantıyı doğrular.
func Redis(ctx context.Context, addr string) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return rdb, nil
}

// Hostname, tüketici adı için kullanılır.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// Fatal, hatayı loglayıp çıkar.
func Fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
