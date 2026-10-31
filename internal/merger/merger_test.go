package merger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nisaguven/artci/internal/match"
	"github.com/nisaguven/artci/internal/normalize"
)

// ---- test yardımcıları ----

type harness struct {
	t   *testing.T
	svc *Service
	ms  *memStore
	pub *memPub
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ms, pub := newMemStore(), &memPub{}
	svc := New(ms, pub, match.DefaultConfig())
	n := 0
	svc.NewID = func() string { n++; return fmt.Sprintf("ev-%03d", n) }
	svc.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return &harness{t: t, svc: svc, ms: ms, pub: pub}
}

func (h *harness) handle(r normalize.Report) Decision {
	h.t.Helper()
	d, err := h.svc.Handle(context.Background(), r)
	if err != nil {
		h.t.Fatalf("Handle(%s/%s): %v", r.Source, r.SourceEventID, err)
	}
	return d
}

// eventView, bir olayın sıradan bağımsız karşılaştırılabilir özetidir.
// ID ve version bilerek yok: farklı geliş sıraları farklı uuid ve ara sürümler
// üretir; önemli olan hangi raporların birlikte olduğu ve gösterilen değerler.
type eventView struct {
	Members         string
	OriginTime      time.Time
	Lat, Lon        float64
	Depth           string
	Magnitude       float64
	MagType         string
	PlaceName       string
	Sources         string
	FirstReportedAt time.Time
}

func (h *harness) snapshot() []eventView {
	h.ms.mu.Lock()
	defer h.ms.mu.Unlock()
	var out []eventView
	for id := range h.ms.events {
		ev := h.ms.event(id)
		var mem []string
		for k, r := range h.ms.reports {
			if r.EventID == id {
				mem = append(mem, k)
			}
		}
		sort.Strings(mem)
		depth := "-"
		if ev.DepthKm != nil {
			depth = fmt.Sprintf("%.1f", *ev.DepthKm)
		}
		out = append(out, eventView{
			Members: strings.Join(mem, ","), OriginTime: ev.OriginTime, Lat: ev.Lat, Lon: ev.Lon,
			Depth: depth, Magnitude: ev.Magnitude, MagType: ev.MagType, PlaceName: ev.PlaceName,
			Sources: strings.Join(ev.Sources, ","), FirstReportedAt: ev.FirstReportedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Members < out[j].Members })
	return out
}

func (h *harness) eventCount() int { return len(h.ms.events) }

// onlyEvent, tek olay olduğunu doğrular ve onu döner.
func (h *harness) onlyEvent() normalize.Event {
	h.t.Helper()
	if n := h.eventCount(); n != 1 {
		h.t.Fatalf("tek olay bekleniyordu, %d var: %+v", n, h.snapshot())
	}
	for id := range h.ms.events {
		return h.ms.event(id)
	}
	panic("ulaşılamaz")
}

var t0 = time.Date(2026, 10, 30, 12, 4, 31, 0, time.UTC)

func fptr(f float64) *float64 { return &f }

func rep(src, id string, dtSec, lat, lon, mag float64) normalize.Report {
	return normalize.Report{
		Source: src, SourceEventID: id,
		OriginTime: t0.Add(time.Duration(dtSec * float64(time.Second))),
		Lat:        lat, Lon: lon, Magnitude: mag, DepthKm: fptr(10),
		ReceivedAt: t0.Add(time.Minute + time.Duration(dtSec*float64(time.Second))),
	}
}

// loadSindirgi, Plan §1'deki temsilî üç kaydı testdata'dan okur.
func loadSindirgi(t *testing.T) (afad, kandilli, emsc normalize.Report) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/match/sindirgi.json")
	if err != nil {
		t.Fatal(err)
	}
	var rs []normalize.Report
	if err := json.Unmarshal(b, &rs); err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("3 rapor bekleniyordu, %d", len(rs))
	}
	for _, r := range rs {
		if err := r.Validate(); err != nil {
			t.Fatalf("testdata geçersiz: %v", err)
		}
	}
	return rs[0], rs[1], rs[2]
}

// permutations, xs'in tüm sıralamalarını üretir (Heap algoritması).
func permutations[T any](xs []T) [][]T {
	var out [][]T
	a := slices.Clone(xs)
	var gen func(int)
	gen = func(k int) {
		if k == 1 {
			out = append(out, slices.Clone(a))
			return
		}
		for i := 0; i < k; i++ {
			gen(k - 1)
			if k%2 == 0 {
				a[i], a[k-1] = a[k-1], a[i]
			} else {
				a[0], a[k-1] = a[k-1], a[0]
			}
		}
	}
	gen(len(a))
	return out
}

// ---- Plan §8 "Zor durumlar" tablosu ----

func TestPlan8_SameSourceTwoIDs_SameEvent(t *testing.T) {
	h := newHarness(t)
	a1 := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	a2 := rep("afad", "a2", 1, 39.215, 28.152, 4.1) // aynı depremin ikinci kaydı

	if d := h.handle(a1); d != DecisionNewEvent {
		t.Fatalf("ilk rapor: %s", d)
	}
	if d := h.handle(a2); d != DecisionAttached {
		t.Fatalf("aynı kaynağın ikinci id'si bağlanmalı: %s", d)
	}
	ev := h.onlyEvent()
	if len(h.ms.reports) != 2 {
		t.Fatalf("raporlar ayrı kalmalı: %d", len(h.ms.reports))
	}
	if !slices.Equal(ev.Sources, []string{"afad"}) {
		t.Fatalf("kaynaklar: %v", ev.Sources)
	}
}

func TestPlan8_AftershockTwoRealQuakes_TwoEvents(t *testing.T) {
	// 20 sn arayla, ~15 km uzakta iki gerçek deprem. Puan ~0,8 (≥0,6) olduğu
	// halde "bu kaynaktan başka rapor yok" kuralı onları ayrı tutmalı.
	h := newHarness(t)
	a := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	b := rep("afad", "a2", 20, 39.21+0.135, 28.15, 3.8)

	cand := match.CandidateOf("x", []normalize.Report{a})
	if s := match.DefaultConfig().Score(cand, b); s < 0.6 {
		t.Fatalf("senaryo puan eşiğini geçmeli ki kural sınansın; puan=%.2f", s)
	}
	h.handle(a)
	if d := h.handle(b); d != DecisionNewEvent {
		t.Fatalf("ikinci deprem yeni olay olmalı: %s", d)
	}
	if h.eventCount() != 2 {
		t.Fatalf("iki olay bekleniyordu: %+v", h.snapshot())
	}

	// Diğer kaynağın raporları kendi depremlerine (en yüksek puan) gitmeli.
	h.handle(rep("kandilli", "k2", 20.5, 39.21+0.14, 28.16, 3.9))
	h.handle(rep("kandilli", "k1", -0.5, 39.20, 28.16, 4.3))
	got := h.snapshot()
	want := []string{"afad|a1,kandilli|k1", "afad|a2,kandilli|k2"}
	if len(got) != 2 || got[0].Members != want[0] || got[1].Members != want[1] {
		t.Fatalf("üyelik yanlış: %+v", got)
	}
}

func TestPlan8_MagnitudeUpdate_SameEventVersionUp(t *testing.T) {
	h := newHarness(t)
	k := rep("kandilli", "k1", 0, 39.19, 28.17, 4.3)
	h.handle(k)
	ev := h.onlyEvent()
	if ev.Version != 1 || ev.Magnitude != 4.3 {
		t.Fatalf("başlangıç: %+v", ev)
	}
	h.pub.got = nil

	k.Magnitude = 4.1
	k.ReceivedAt = k.ReceivedAt.Add(5 * time.Minute)
	if d := h.handle(k); d != DecisionUpdate {
		t.Fatalf("karar: %s", d)
	}
	ev = h.onlyEvent()
	if ev.Version != 2 || ev.Magnitude != 4.1 {
		t.Fatalf("olay güncellenmeli ve version artmalı: %+v", ev)
	}
	if len(h.pub.got) != 1 {
		t.Fatalf("bir yayın bekleniyordu: %d", len(h.pub.got))
	}
	u := h.pub.got[0]
	if u.IsNew || u.PrevMagnitude == nil || *u.PrevMagnitude != 4.3 || u.Event.Magnitude != 4.1 {
		t.Fatalf("yayın: %+v prev=%v", u, u.PrevMagnitude)
	}
	if !ev.FirstReportedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("FirstReportedAt güncellemeyle değişmemeli: %v", ev.FirstReportedAt)
	}
}

func TestPlan8_DuplicateMessage_Noop(t *testing.T) {
	h := newHarness(t)
	a := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	h.handle(a)
	before := h.snapshot()
	pubs := len(h.pub.got)

	again := a
	again.ReceivedAt = a.ReceivedAt.Add(30 * time.Second) // polling'in sonraki turu
	again.Raw = []byte(`{"yeniden":"cekildi"}`)
	for range 3 {
		if d := h.handle(again); d != DecisionNoop {
			t.Fatalf("aynı mesaj noop olmalı: %s", d)
		}
	}
	if ev := h.onlyEvent(); ev.Version != 1 {
		t.Fatalf("version değişmemeli: %d", ev.Version)
	}
	if !slices.Equal(before, h.snapshot()) || len(h.pub.got) != pubs {
		t.Fatal("hiçbir şey değişmemeli, yayın olmamalı")
	}
}

func TestPlan8_ReverseOrder_SameResult(t *testing.T) {
	afad, kand, emsc := loadSindirgi(t)

	h1 := newHarness(t)
	for _, r := range []normalize.Report{afad, kand, emsc} {
		h1.handle(r)
	}
	h2 := newHarness(t)
	for _, r := range []normalize.Report{emsc, kand, afad} {
		h2.handle(r)
	}
	s1, s2 := h1.snapshot(), h2.snapshot()
	if !slices.Equal(s1, s2) {
		t.Fatalf("sıra sonucu değiştirdi:\n%+v\n%+v", s1, s2)
	}
	ev := h2.onlyEvent()
	if ev.Magnitude != 4.1 || ev.MagType != "Mw" || ev.PlaceName != "Sındırgı (Balıkesir)" {
		t.Fatalf("AFAD değerleri gösterilmeli: %+v", ev)
	}
	if !slices.Equal(ev.Sources, []string{"afad", "kandilli", "emsc"}) {
		t.Fatalf("kaynaklar: %v", ev.Sources)
	}
	if !ev.FirstReportedAt.Equal(emsc.ReceivedAt) {
		t.Fatalf("FirstReportedAt en erken ulaşan (EMSC) olmalı: %v", ev.FirstReportedAt)
	}
}

// ---- Sıra bağımsızlığı: permütasyonlar ----

func TestOrderIndependence_SingleQuakeAllPermutations(t *testing.T) {
	afad, kand, emsc := loadSindirgi(t)
	afadDup := afad // AFAD aynı depremi ikinci bir id ile de yayınladı
	afadDup.SourceEventID = "656322"
	afadDup.OriginTime = afad.OriginTime.Add(time.Second)
	afadDup.ReceivedAt = afad.ReceivedAt.Add(time.Minute)

	reports := []normalize.Report{afad, kand, emsc, afadDup}
	var want []eventView
	perms := permutations(reports)
	if len(perms) != 24 {
		t.Fatalf("24 permütasyon bekleniyordu: %d", len(perms))
	}
	for i, p := range perms {
		h := newHarness(t)
		for _, r := range p {
			h.handle(r)
			h.handle(r) // at-least-once: her mesaj iki kez gelebilir
		}
		got := h.snapshot()
		if i == 0 {
			want = got
			if len(want) != 1 {
				t.Fatalf("tek olay bekleniyordu: %+v", want)
			}
			continue
		}
		if !slices.Equal(got, want) {
			t.Fatalf("permütasyon %d farklı sonuç verdi:\n got %+v\nwant %+v", i, got, want)
		}
	}
	// AFAD'ın iki id'si arasında en son alınan kazanır.
	if want[0].OriginTime != afadDup.OriginTime || want[0].Sources != "afad,kandilli,emsc" {
		t.Fatalf("gösterilen değerler: %+v", want[0])
	}
}

// İki gerçek deprem, iki kaynak (artçı dizisi). Sonuç, her kaynağın kendi
// iki raporunu birbirinden ayırdığı sürece sıradan bağımsızdır. Aşağıdaki
// permütasyonlar bunu sağlar; sağlamayan "çapraz" sıralar için bkz.
// TestKnownLimitation_CrossedAftershocks.
func TestOrderIndependence_AftershockPermutations(t *testing.T) {
	a1 := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	a2 := rep("afad", "a2", 20, 39.21+0.135, 28.15, 3.8)
	k1 := rep("kandilli", "k1", -0.5, 39.20, 28.16, 4.3)
	k2 := rep("kandilli", "k2", 20.5, 39.21+0.14, 28.16, 3.9)
	want := []string{"afad|a1,kandilli|k1", "afad|a2,kandilli|k2"}

	ok, crossed := 0, 0
	for _, p := range permutations([]normalize.Report{a1, a2, k1, k2}) {
		h := newHarness(t)
		for _, r := range p {
			h.handle(r)
		}
		got := h.snapshot()
		if len(got) != 2 {
			t.Fatalf("her sırada iki olay olmalı; sıra %v -> %+v", ids(p), got)
		}
		if got[0].Members == want[0] && got[1].Members == want[1] {
			ok++
			continue
		}
		crossed++
		// Çapraz durum yalnızca ilk iki raporun farklı kaynak ve farklı
		// depremden olduğu sıralarda oluşabilir.
		if p[0].Source == p[1].Source || sameQuake(p[0], p[1]) {
			t.Fatalf("beklenmeyen çapraz eşleşme; sıra %v -> %+v", ids(p), got)
		}
	}
	t.Logf("24 sıra: doğru=%d, çapraz=%d (belgelenmiş sınır)", ok, crossed)
}

func ids(rs []normalize.Report) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.SourceEventID)
	}
	return out
}

func sameQuake(a, b normalize.Report) bool { return a.SourceEventID[1:] == b.SourceEventID[1:] }

// Açgözlü (online) eşleştirmenin bilinen sınırı: önce Kandilli-1 gelir, sonra
// AFAD-2. Elinde yalnızca 1. deprem olan olay, AFAD-2 için tek ve geçerli aday
// (puan ~0,8, olayda AFAD yok) -> bağlanır. Olay sayısı yine doğru (2) kalır ama
// üyelik çaprazlanır. Bunu düzeltmek yeniden kümeleme ister; MVP'de bilinçli
// olarak yapılmadı (docs/MATCHING.md).
func TestKnownLimitation_CrossedAftershocks(t *testing.T) {
	h := newHarness(t)
	k1 := rep("kandilli", "k1", -0.5, 39.20, 28.16, 4.3)
	a2 := rep("afad", "a2", 20, 39.21+0.135, 28.15, 3.8)
	h.handle(k1)
	if d := h.handle(a2); d != DecisionAttached {
		t.Fatalf("sınır değişti mi? karar=%s (değiştiyse dokümanı güncelle)", d)
	}
}

// ---- Güncelleme kenar durumları ----

func TestUpdateWithoutVisibleChange_NoVersionBump(t *testing.T) {
	afad, _, emsc := loadSindirgi(t)
	h := newHarness(t)
	h.handle(afad)
	h.handle(emsc)
	ev := h.onlyEvent()
	pubs := len(h.pub.got)

	emsc.Magnitude = 4.0 // AFAD öncelikli: gösterilen değer değişmez
	if d := h.handle(emsc); d != DecisionUpdate {
		t.Fatalf("karar: %s", d)
	}
	if got := h.onlyEvent(); got.Version != ev.Version || got.Magnitude != 4.1 {
		t.Fatalf("gösterilen değer değişmediyse version artmamalı: %+v", got)
	}
	if len(h.pub.got) != pubs {
		t.Fatal("değişmeyen olay yayınlanmamalı")
	}
}

func TestUpdateSplitsEvent(t *testing.T) {
	afad, _, emsc := loadSindirgi(t)
	h := newHarness(t)
	h.handle(afad)
	h.handle(emsc)
	orig := h.onlyEvent()
	h.pub.got = nil

	// EMSC konumu ~200 km düzeltti: artık bu olaya ait değil.
	emsc.Lat += 1.8
	if d := h.handle(emsc); d != DecisionUpdate {
		t.Fatalf("karar: %s", d)
	}
	got := h.snapshot()
	if len(got) != 2 || got[0].Members != "afad|656321" || got[1].Members != "emsc|20261030_0000123" {
		t.Fatalf("olay bölünmeliydi: %+v", got)
	}
	old := h.ms.event(orig.ID)
	if old.Version != orig.Version+1 || !slices.Equal(old.Sources, []string{"afad"}) {
		t.Fatalf("eski olay kalan raporla yeniden hesaplanmalı: %+v", old)
	}
	// İki yayın: eski olayın güncellemesi + yeni olay.
	var news, updates int
	for _, u := range h.pub.got {
		if u.IsNew {
			news++
		} else {
			updates++
		}
	}
	if news != 1 || updates != 1 {
		t.Fatalf("yayınlar: yeni=%d güncelleme=%d", news, updates)
	}
}

func TestUpdateOfLoneReport_MovesEvent(t *testing.T) {
	h := newHarness(t)
	e := rep("emsc", "e1", 0, 39.21, 28.15, 4.2)
	h.handle(e)
	e.Lat += 1.8 // tek başına: bölünecek bir şey yok, olay taşınır
	h.handle(e)
	if ev := h.onlyEvent(); ev.Lat != e.Lat || ev.Version != 2 {
		t.Fatalf("olay raporla birlikte taşınmalı: %+v", ev)
	}
}

// ---- Güvenilirlik ----

func TestRollbackOnFailure_RetryIsClean(t *testing.T) {
	h := newHarness(t)
	a := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	h.ms.failTx = errors.New("bağlantı koptu")
	if _, err := h.svc.Handle(context.Background(), a); err == nil {
		t.Fatal("hata bekleniyordu")
	}
	if len(h.ms.reports) != 0 || h.eventCount() != 0 || len(h.pub.got) != 0 {
		t.Fatal("geri alma sonrası iz kalmamalı")
	}
	h.ms.failTx = nil
	if d := h.handle(a); d != DecisionNewEvent {
		t.Fatalf("yeniden deneme temiz başlamalı: %s", d)
	}
}

func TestPublishFailure_RecoveredOnRedelivery(t *testing.T) {
	h := newHarness(t)
	a := rep("afad", "a1", 0, 39.21, 28.15, 4.1)
	h.pub.fail = errors.New("redis yok")
	_, err := h.svc.Handle(context.Background(), a)
	if !errors.Is(err, ErrPublish) {
		t.Fatalf("ErrPublish bekleniyordu: %v", err)
	}
	// DB commit edildi; mesaj ACK'lenmedi ve yeniden geliyor.
	h.pub.fail = nil
	if d := h.handle(a); d != DecisionNoop {
		t.Fatalf("yeniden teslim noop olmalı: %s", d)
	}
	if len(h.pub.got) != 1 || !h.pub.got[0].IsNew {
		t.Fatalf("kaçan yayın tamamlanmalı: %+v", h.pub.got)
	}
	h.handle(a)
	if len(h.pub.got) != 1 {
		t.Fatal("yayın bir kez tamamlandıktan sonra tekrarlanmamalı")
	}
}

func TestPublishedSequence(t *testing.T) {
	afad, kand, emsc := loadSindirgi(t)
	h := newHarness(t)
	h.handle(emsc)
	h.handle(kand)
	h.handle(afad)
	if len(h.pub.got) != 3 {
		t.Fatalf("3 yayın bekleniyordu: %d", len(h.pub.got))
	}
	first, last := h.pub.got[0], h.pub.got[2]
	if !first.IsNew || first.PrevMagnitude != nil || first.Event.Version != 1 {
		t.Fatalf("ilk yayın yeni olay olmalı: %+v", first)
	}
	if last.IsNew || *last.PrevMagnitude != 4.3 || last.Event.Magnitude != 4.1 || last.Event.Version != 3 {
		t.Fatalf("son yayın: %+v prev=%v", last, *last.PrevMagnitude)
	}
}
