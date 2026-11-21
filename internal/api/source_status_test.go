package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSourceStatusesFreshnessAndRecovery(t *testing.T) {
	var kandilliLast atomic.Int64
	kandilliLast.Store(testNow.Add(-15 * time.Minute).Unix())
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") != sourceStatusQuery {
			t.Errorf("beklenmeyen Prometheus isteği: %s", r.URL)
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"source":"afad"},"value":[%d,"%d"]},
			{"metric":{"source":"kandilli"},"value":[%d,"%d"]}
		]}}`, testNow.Unix(), testNow.Add(-14*time.Minute-59*time.Second).Unix(), testNow.Unix(), kandilliLast.Load())
	}))
	defer prometheus.Close()
	handler := (&Server{PrometheusURL: prometheus.URL, Now: func() time.Time { return testNow }}).Handler()

	for _, wantKandilli := range []string{"stale", "recent"} {
		rec, _ := do(t, handler, "/api/sources")
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("kaynak uç yanıtı: %d, %v", rec.Code, rec.Header())
		}
		var body struct{ Sources []sourceStatus }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		sources := body.Sources
		if len(sources) != 3 || sources[0].Source != "afad" || sources[0].Status != "recent" ||
			sources[1].Source != "kandilli" || sources[1].Status != wantKandilli ||
			sources[2].Source != "emsc" || sources[2].Status != "unknown" || sources[2].LastSuccess != nil {
			t.Fatalf("beklenmeyen kaynak durumları: %+v", sources)
		}
		if last := sources[1].LastSuccess; last == nil || last.Unix() != kandilliLast.Load() {
			t.Fatalf("Kandilli son erişimi: %v", last)
		}
		kandilliLast.Store(testNow.Unix())
	}
}

func TestSourceStatusesUnavailable(t *testing.T) {
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer prometheus.Close()
	for _, endpoint := range []string{"", prometheus.URL} {
		handler := (&Server{PrometheusURL: endpoint}).Handler()
		rec, _ := do(t, handler, "/api/sources")
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("kaynak uç yanıtı: %d, %v", rec.Code, rec.Header())
		}
	}
}
