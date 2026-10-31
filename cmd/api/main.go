// api: REST uçları, canlı harita için WebSocket yayını ve gömülü web sitesi.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nisaguven/artci/internal/api"
	"github.com/nisaguven/artci/internal/app"
	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/store"
	"github.com/nisaguven/artci/web"
)

func main() {
	ctx, stop := app.Init("api")
	defer stop()

	cfg := app.LoadConfig(":9105")
	addr := app.Env("ARTCI_HTTP_ADDR", ":8080")

	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		app.Fatal("postgres", err)
	}
	defer st.Close()
	rdb, err := app.Redis(ctx, cfg.RedisAddr)
	if err != nil {
		app.Fatal("redis", err)
	}
	defer rdb.Close()

	metrics.Serve(ctx, cfg.MetricsAddr)

	hub := api.NewHub(api.HubConfig{})
	srv := &http.Server{
		Addr:    addr,
		Handler: (&api.Server{Store: st, Static: web.FS, WS: hub}).Handler(),
		// Sunucu genelinde Read/WriteTimeout yok: uzun ömürlü WebSocket
		// bağlantılarını keserdi. Normal istekler TimeoutHandler ile sınırlı.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	// Shutdown ele geçirilmiş WS bağlantılarını kapatmaz; hub'a biz söyleriz.
	srv.RegisterOnShutdown(hub.Close)

	var wg sync.WaitGroup
	wg.Go(func() { hub.Run(ctx) })
	wg.Go(func() {
		name := app.Hostname()
		for ctx.Err() == nil {
			if err := api.ConsumeUpdates(ctx, rdb, hub, name); err != nil {
				slog.Error("events.updated tüketimi", "err", err)
			}
			// Redis geçici olarak düştüyse sıkı döngüye girmeden yeniden dene.
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	})

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("api dinliyor", "addr", addr, "metrics", cfg.MetricsAddr)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http sunucusu", "err", err)
		}
	}

	slog.Info("kapanıyor")
	stop()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		slog.Warn("http kapanışı", "err", err)
	}
	if err := hub.Wait(sctx); err != nil {
		slog.Warn("ws bağlantıları zamanında kapanmadı", "err", err)
	}
	wg.Wait()
}
