// Package metrics, Plan §12'deki tüm Prometheus metriklerini tek yerde tanımlar.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var latencyBuckets = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1200}

var (
	IngestReports = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "artci_ingest_reports_total", Help: "Kaynak başına gelen rapor sayısı",
	}, []string{"source"})
	IngestLag = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "artci_ingest_lag_seconds", Help: "Depremin olduğu an -> raporun bize ulaştığı an",
		Buckets: latencyBuckets,
	}, []string{"source"})
	SourceUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "artci_source_up", Help: "Kaynak erişilebilir mi (1/0)",
	}, []string{"source"})
	SourceLastSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "artci_source_last_success_timestamp_seconds", Help: "Kaynaktan son başarılı okuma zamanı",
	}, []string{"source"})
	MergeDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "artci_merge_decisions_total", Help: "new_event / attached / update / noop",
	}, []string{"decision"})
	DLQMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "artci_dlq_messages_total", Help: "Ayrıştırılamayan kayıtlar",
	}, []string{"source"})
	NotifyLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "artci_notify_latency_seconds", Help: "İlk rapor -> bildirim gönderimi",
		Buckets: latencyBuckets,
	})
	NotificationsSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "artci_notifications_total", Help: "Bildirim sonuçları (sent / duplicate / error)",
	}, []string{"result"})
	SummaryResults = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "artci_summary_results_total", Help: "llm_ok / rejected / fallback",
	}, []string{"result"})
	LLMCost = promauto.NewCounter(prometheus.CounterOpts{
		Name: "artci_llm_cost_usd_total", Help: "Toplam LLM harcaması (USD)",
	})
	WSClients = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "artci_ws_clients", Help: "Bağlı WebSocket istemcisi",
	})
)

// Serve, /metrics ve /healthz uçlarını addr üzerinde açar; ctx bitince kapanır.
func Serve(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server", "err", err)
		}
	}()
}
