package summary

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

type memStore struct {
	rows   map[string]Record
	spent  map[string]float64
	failDB bool
}

func newMem() *memStore { return &memStore{rows: map[string]Record{}, spent: map[string]float64{}} }

func key(id string, v int) string { return id + "#" + string(rune('0'+v)) }

func (m *memStore) HasSummary(_ context.Context, id string, v int) (bool, error) {
	if m.failDB {
		return false, errors.New("db yok")
	}
	for _, r := range m.rows {
		if r.EventID == id && r.EventVersion >= v {
			return true, nil
		}
	}
	return false, nil
}
func (m *memStore) SaveSummary(_ context.Context, r Record) error {
	if _, ok := m.rows[key(r.EventID, r.EventVersion)]; !ok {
		m.rows[key(r.EventID, r.EventVersion)] = r
	}
	return nil
}
func (m *memStore) SpentOn(_ context.Context, d time.Time) (float64, error) {
	return m.spent[d.Format("2006-01-02")], nil
}
func (m *memStore) AddSpend(_ context.Context, d time.Time, usd float64) error {
	m.spent[d.Format("2006-01-02")] += usd
	return nil
}

type fakeLLM struct {
	out   string
	err   error
	cost  float64
	calls int
}

func (f *fakeLLM) Name() string { return "llm:fake" }
func (f *fakeLLM) Generate(context.Context, Facts) (string, float64, error) {
	f.calls++
	return f.out, f.cost, f.err
}

func svc(st Store, llm Generator) *Service {
	s := &Service{Store: st, Cfg: DefaultConfig(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC) }}
	if llm != nil {
		s.LLM = llm
	}
	return s
}

func upd(ev normalize.Event) normalize.EventUpdate { return normalize.EventUpdate{Event: ev} }

func TestService_LLMAccepted(t *testing.T) {
	st := newMem()
	llm := &fakeLLM{out: `{"ozet": "Sındırgı (Balıkesir) merkezli 4,1 büyüklüğünde bir deprem saat 15:04'te meydana geldi."}`, cost: 0.001}
	out, err := svc(st, llm).Handle(context.Background(), upd(sindirgi()))
	if err != nil || out != OutcomeLLM {
		t.Fatalf("%v %v", out, err)
	}
	r := st.rows[key("E1", 1)]
	if r.Generator != "llm:fake" || !r.Validated || !strings.Contains(r.Text, "4,1") {
		t.Errorf("kayıt yanlış: %+v", r)
	}
	if st.spent["2026-10-30"] != 0.001 {
		t.Errorf("harcama kaydedilmeli: %v", st.spent)
	}
	// Aynı sürüm ikinci kez: önbellekten, LLM çağrılmaz.
	out, _ = svc(st, llm).Handle(context.Background(), upd(sindirgi()))
	if out != OutcomeSkippedCached || llm.calls != 1 {
		t.Errorf("önbellek: %v calls=%d", out, llm.calls)
	}
}

func TestService_RejectedFallsBackToTemplate(t *testing.T) {
	st := newMem()
	llm := &fakeLLM{out: `{"ozet": "Sındırgı merkezli 4,1 büyüklüğünde bir deprem oldu, daha büyüğü gelebilir."}`}
	out, err := svc(st, llm).Handle(context.Background(), upd(sindirgi()))
	if err != nil || out != OutcomeFallback {
		t.Fatalf("%v %v", out, err)
	}
	r := st.rows[key("E1", 1)]
	if r.Generator != "template" || !r.Validated || strings.Contains(r.Text, "büyüğü") {
		t.Errorf("şablona düşmeli: %+v", r)
	}
}

func TestService_LLMErrorAndNoLLM(t *testing.T) {
	for name, llm := range map[string]Generator{"hata": &fakeLLM{err: errors.New("timeout")}, "yok": nil} {
		st := newMem()
		out, err := svc(st, llm).Handle(context.Background(), upd(sindirgi()))
		if err != nil || out != OutcomeFallback || st.rows[key("E1", 1)].Generator != "template" {
			t.Errorf("%s: %v %v %+v", name, out, err, st.rows)
		}
	}
}

func TestService_BudgetCap(t *testing.T) {
	st := newMem()
	st.spent["2026-10-30"] = 1.0
	llm := &fakeLLM{out: `{"ozet":"x"}`}
	out, _ := svc(st, llm).Handle(context.Background(), upd(sindirgi()))
	if out != OutcomeFallback || llm.calls != 0 {
		t.Errorf("tavan aşılınca LLM çağrılmamalı: %v calls=%d", out, llm.calls)
	}
}

func TestService_SmallAndOlderVersion(t *testing.T) {
	st := newMem()
	ev := sindirgi()
	ev.Magnitude = 3.4
	if out, _ := svc(st, nil).Handle(context.Background(), upd(ev)); out != OutcomeSkippedSmall {
		t.Errorf("M<3,5 atlanmalı: %v", out)
	}
	ev = sindirgi()
	ev.Version = 3
	svc(st, nil).Handle(context.Background(), upd(ev))
	ev.Version = 2 // geç gelen eski sürüm
	if out, _ := svc(st, nil).Handle(context.Background(), upd(ev)); out != OutcomeSkippedCached {
		t.Errorf("daha yeni sürümün özeti varken eski sürüm atlanmalı: %v", out)
	}
}

func TestService_DBErrorIsReturned(t *testing.T) {
	st := newMem()
	st.failDB = true
	if _, err := svc(st, nil).Handle(context.Background(), upd(sindirgi())); err == nil {
		t.Error("DB hatası çağırana dönmeli (mesaj ACK'lenmesin)")
	}
}

func TestOpenAICompatible(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"ozet\":\"metin\"}"}}],
			"usage":{"prompt_tokens":1000,"completion_tokens":100}}`))
	}))
	defer srv.Close()
	o := &OpenAICompatible{URL: srv.URL, APIKey: "k", Model: "m", InputPerM: 1, OutputPerM: 10}
	raw, cost, err := o.Generate(context.Background(), BuildFacts(sindirgi()))
	if err != nil || raw != `{"ozet":"metin"}` {
		t.Fatalf("%q %v", raw, err)
	}
	if cost < 0.00199 || cost > 0.00201 {
		t.Errorf("maliyet 1000*1/1e6 + 100*10/1e6 = 0,002 olmalı: %v", cost)
	}
	if gotAuth != "Bearer k" || !strings.Contains(gotBody, `"json_object"`) || !strings.Contains(gotBody, `\"buyukluk\":\"4,1\"`) {
		t.Errorf("istek yanlış: %s %s", gotAuth, gotBody)
	}
	if strings.Contains(gotBody, "magValue") {
		t.Error("iç alanlar modele gönderilmemeli")
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv2.Close()
	o.URL = srv2.URL
	if _, _, err := o.Generate(context.Background(), BuildFacts(sindirgi())); err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("HTTP hatası dönmeli: %v", err)
	}
}
