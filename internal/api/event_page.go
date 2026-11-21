package api

import (
	"bytes"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/store"
)

//go:embed event_page.html
var eventPageHTML string

var eventPageTemplate = template.Must(template.New("deprem").Funcs(template.FuncMap{
	"decimal": func(n float64) string {
		return strings.Replace(strconv.FormatFloat(n, 'f', 1, 64), ".", ",", 1)
	},
	"turkeyTime": func(t time.Time) string {
		return t.In(eventPageLocation).Format("02.01.2006 15:04:05")
	},
	"sourceName": func(source string) string {
		switch source {
		case normalize.SourceAFAD:
			return "AFAD"
		case normalize.SourceKandilli:
			return "Kandilli Rasathanesi"
		case normalize.SourceEMSC:
			return "EMSC"
		}
		return source
	},
}).Parse(eventPageHTML))

var eventPageLocation = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		return time.FixedZone("TRT", 3*60*60)
	}
	return loc
}()

func (s *Server) eventPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Title   string
		Message string
		Event   *store.EventDetailView
	}{Title: "Deprem bulunamadı", Message: "Bu bağlantıyla eşleşen bir kayıt yok."}
	status := http.StatusNotFound
	id := r.PathValue("id")
	if uuidRe.MatchString(id) {
		event, err := s.Store.EventDetail(r.Context(), id)
		switch {
		case err == nil:
			status = http.StatusOK
			data.Event, data.Title = event, event.PlaceName
			if data.Title == "" {
				data.Title = "Deprem ayrıntısı"
			}
		case !errors.Is(err, store.ErrNotFound):
			slog.Error("deprem sayfası", "id", id, "err", err)
			status = http.StatusServiceUnavailable
			data.Title = "Deprem ayrıntısı yüklenemedi"
			data.Message = "Biraz sonra sayfayı yenileyerek tekrar deneyebilirsiniz."
		}
	}

	var body bytes.Buffer
	if err := eventPageTemplate.Execute(&body, data); err != nil {
		slog.Error("deprem sayfası şablonu", "err", err)
		http.Error(w, "Deprem ayrıntısı yüklenemedi", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = body.WriteTo(w)
}
