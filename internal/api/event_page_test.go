package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/store"
)

func TestEventPageRendersArchivedEvent(t *testing.T) {
	depth, summary := 6.2, "Özet <b>metni</b>"
	detail := &store.EventDetailView{
		EventView: store.EventView{
			ID: testID, OriginTime: time.Date(2026, 8, 10, 9, 4, 0, 0, time.UTC),
			PlaceName: "Sındırgı <b>Merkez</b>", Magnitude: 4.3, MagType: "ML", DepthKm: &depth, Summary: &summary,
		},
		Reports: []store.ReportView{{Source: "kandilli", Magnitude: 4.1, MagType: "ML"}},
	}
	h := newTestServer(&fakeStore{detail: detail})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/deprem/"+testID, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("deprem sayfası: %d %v\n%s", rec.Code, rec.Header(), rec.Body)
	}
	for _, want := range []string{
		"Sındırgı &lt;b&gt;Merkez&lt;/b&gt;", "10.08.2026 12:04:00", "4,3 (ML)", "6,2 km",
		"Özet &lt;b&gt;metni&lt;/b&gt;", "Kandilli Rasathanesi · 4,1 (ML)",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("sayfada bulunamadı: %q", want)
		}
	}
}

func TestEventPageErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		err    error
		status int
		text   string
	}{
		{"invalid", "gecersiz", errors.New("depo erişilmemeli"), http.StatusNotFound, "Deprem bulunamadı"},
		{"missing", testID, nil, http.StatusNotFound, "Deprem bulunamadı"},
		{"unavailable", testID, errors.New("depo hatası"), http.StatusServiceUnavailable, "Deprem ayrıntısı yüklenemedi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestServer(&fakeStore{err: tc.err})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/deprem/"+tc.id, nil))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.text) ||
				strings.Contains(rec.Body.String(), "depo hatası") {
				t.Fatalf("deprem sayfası: %d\n%s", rec.Code, rec.Body)
			}
		})
	}
}
