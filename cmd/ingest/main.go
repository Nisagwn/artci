// Command ingest, tek bir kaynaktan deprem raporlarını toplayıp ortak biçime
// çevirir ve reports.raw akışına yazar (Plan §6). Her kaynak ayrı süreçtir;
// biri çökerse diğerleri etkilenmez.
//
//	go run ./cmd/ingest --source=afad
//	go run ./cmd/ingest --source=kandilli
//	go run ./cmd/ingest --source=emsc
//
// Ortam değişkenleri (ortak olanlar internal/app'te):
//
//	ARTCI_POLL_INTERVAL     afad/kandilli sorgu aralığı (varsayılan 30s; Plan §5: 30–60 sn)
//	ARTCI_USER_AGENT        isteklerde gönderilen User-Agent (iletişim bilgisi içermeli)
//	ARTCI_AFAD_URL          AFAD API adresi
//	ARTCI_KANDILLI_URL      Kandilli sayfası adresi
//	ARTCI_EMSC_WS_URL       EMSC WebSocket adresi
//	ARTCI_EMSC_FDSN_URL     EMSC FDSN adresi (boşluk doldurma)
//	ARTCI_BREAKER_THRESHOLD breaker'ı açan ardışık hata sayısı (varsayılan 5)
//	ARTCI_BREAKER_COOLDOWN  breaker açık kalma süresi (varsayılan 5m)
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nisaguven/artci/internal/app"
	"github.com/nisaguven/artci/internal/metrics"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/source"
)

// Kaynak başına metrik portu: aynı makinede üçü birlikte çalışabilsin.
var metricsPorts = map[string]string{
	normalize.SourceAFAD:     ":9111",
	normalize.SourceKandilli: ":9112",
	normalize.SourceEMSC:     ":9113",
}

func main() {
	src := flag.String("source", "", "afad | kandilli | emsc")
	flag.Parse()
	port, ok := metricsPorts[*src]
	if !ok {
		fmt.Fprintln(os.Stderr, "kullanım: ingest --source=afad|kandilli|emsc")
		os.Exit(2)
	}

	ctx, stop := app.Init("ingest-" + *src)
	defer stop()
	cfg := app.LoadConfig(port)

	rdb, err := app.Redis(ctx, cfg.RedisAddr)
	if err != nil {
		app.Fatal("redis", err)
	}
	defer rdb.Close()
	metrics.Serve(ctx, cfg.MetricsAddr)

	ua := app.Env("ARTCI_USER_AGENT", source.UserAgent)
	hc := source.NewHTTPClient(ua)
	em := &source.Emitter{Source: *src, Sink: source.StreamSink{RDB: rdb}}
	bo := source.Backoff{Base: 2 * time.Second, Max: 2 * time.Minute}

	slog.Info("toplayıcı başladı", "source", *src)
	switch *src {
	case normalize.SourceEMSC:
		s := &source.EMSCStream{
			URL:       app.Env("ARTCI_EMSC_WS_URL", source.EMSCWebSocketURL),
			Filler:    &source.EMSCFDSN{HTTP: hc, BaseURL: app.Env("ARTCI_EMSC_FDSN_URL", source.EMSCFDSNURL)},
			Emitter:   em,
			Backoff:   bo,
			UserAgent: ua,
		}
		s.Run(ctx)
	default:
		var f source.Fetcher
		if *src == normalize.SourceAFAD {
			f = &source.AFAD{HTTP: hc, BaseURL: app.Env("ARTCI_AFAD_URL", source.AFADBaseURL)}
		} else {
			f = &source.Kandilli{HTTP: hc, URL: app.Env("ARTCI_KANDILLI_URL", source.KandilliURL)}
		}
		p := &source.Poller{
			Fetcher:  f,
			Emitter:  em,
			Interval: app.EnvDuration("ARTCI_POLL_INTERVAL", 30*time.Second),
			Backoff:  bo,
			Breaker: &source.Breaker{
				Threshold: app.EnvInt("ARTCI_BREAKER_THRESHOLD", 5),
				Cooldown:  app.EnvDuration("ARTCI_BREAKER_COOLDOWN", 5*time.Minute),
			},
		}
		p.Run(ctx)
	}
}
