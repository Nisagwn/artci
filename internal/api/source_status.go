package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

const sourceStatusQuery = `max by (source) (artci_source_last_success_timestamp_seconds{job="artci",source=~"afad|kandilli|emsc"})`

const sourceStaleAfter = 15 * time.Minute

var sourceStatusClient = &http.Client{Timeout: 2 * time.Second}

type sourceStatus struct {
	Source      string     `json:"source"`
	Status      string     `json:"status"`
	LastSuccess *time.Time `json:"last_success"`
}

func (s *Server) sourceStatuses(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.PrometheusURL == "" {
		writeError(w, http.StatusServiceUnavailable, "kaynak durumu yapılandırılmamış")
		return
	}
	lastSuccess, err := fetchSourceLastSuccess(r.Context(), s.PrometheusURL)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "kaynak durumu alınamadı")
		return
	}

	now := s.Now().UTC()
	statuses := make([]sourceStatus, 0, 3)
	for _, source := range []string{normalize.SourceAFAD, normalize.SourceKandilli, normalize.SourceEMSC} {
		status := sourceStatus{Source: source, Status: "unknown"}
		seconds := lastSuccess[source]
		if seconds > 0 && seconds <= float64(now.Add(30*time.Second).Unix()) {
			last := time.Unix(int64(seconds), 0).UTC()
			status.LastSuccess = &last
			status.Status = "recent"
			if now.Sub(last) >= sourceStaleAfter {
				status.Status = "stale"
			}
		}
		statuses = append(statuses, status)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": statuses})
}

func fetchSourceLastSuccess(ctx context.Context, baseURL string) (map[string]float64, error) {
	endpoint, err := url.JoinPath(baseURL, "api/v1/query")
	if err != nil {
		return nil, err
	}
	query := url.Values{"query": {sourceStatusQuery}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := sourceStatusClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus HTTP %d", res.StatusCode)
	}

	var payload struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric struct {
					Source string `json:"source"`
				} `json:"metric"`
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Status != "success" || payload.Data.ResultType != "vector" {
		return nil, fmt.Errorf("beklenmeyen Prometheus yanıtı")
	}
	lastSuccess := make(map[string]float64, len(payload.Data.Result))
	for _, sample := range payload.Data.Result {
		var value string
		if len(sample.Value) == 2 && json.Unmarshal(sample.Value[1], &value) == nil {
			if seconds, err := strconv.ParseFloat(value, 64); err == nil {
				lastSuccess[sample.Metric.Source] = seconds
			}
		}
	}
	return lastSuccess, nil
}
