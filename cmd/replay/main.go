// Command replay, bir deprem dizisini hatta hızlandırarak yeniden oynatır ve
// birleştirme doğruluğu ile gecikmeyi ölçer (Plan §13).
//
//	replay gen -out testdata/replay/sindirgi-sentetik.jsonl      # sentetik dizi üret
//	replay run -in testdata/replay/sindirgi-sentetik.jsonl -speed 10
//	replay run -in ... -speed 0                                  # olabildiğince hızlı (iş hacmi)
//	replay run -in testdata/replay/sindirgi-isc-2025-08.jsonl.gz -speed 0 # gerçek dizi
//	replay isc -out testdata/replay/sindirgi-isc-2025-08.jsonl.gz # ISC bülteninden gerçek dizi
//
// run, raporları reports.raw akışına yazar; merger'ın çalışıyor olması gerekir.
// Ölçülenler:
//   - iş hacmi: merger'ın işlediği rapor/sn (yalnızca -speed 0 anlamlıdır)
//   - doğruluk: çift tabanlı precision/recall (bkz. internal/replay.Score)
//   - gecikme: bir olayın ilk raporunun akışa yazılması -> olayın events.updated'da
//     görünmesi (p50/p95/p99). Kaynakların kendi gecikmesi dahil değildir.
//
// UYARI: Oynatılan depremler gerçek olay gibi işlenir. notifier aynı Redis'e
// bağlıysa abonelere sahte bildirim gider; bu yüzden events.updated üzerinde
// "notifier" grubu varsa run, -force verilmedikçe çalışmaz. Ayrı bir ortamda kullanın.
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nisaguven/artci/internal/app"
	"github.com/nisaguven/artci/internal/normalize"
	"github.com/nisaguven/artci/internal/replay"
	"github.com/nisaguven/artci/internal/source"
	"github.com/nisaguven/artci/internal/store"
	"github.com/nisaguven/artci/internal/stream"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = gen(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "isc":
		err = isc(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hata:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "kullanım: replay gen [-out dosya] | replay isc [-out dosya] | replay run -in dosya [-speed 10]")
	os.Exit(2)
}

func gen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	c := replay.DefaultGenConfig()
	out := fs.String("out", "-", "çıktı dosyası (- = stdout)")
	fs.Uint64Var(&c.Seed, "seed", c.Seed, "rastgelelik tohumu")
	fs.IntVar(&c.Count, "count", c.Count, "artçı sayısı")
	fs.DurationVar(&c.Duration, "duration", c.Duration, "dizi süresi")
	fs.Parse(args)

	entries := replay.Generate(c)
	w := os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if err := replay.WriteCatalog(w, entries); err != nil {
		return err
	}
	truths := map[string]bool{}
	for _, e := range entries {
		truths[e.Truth] = true
	}
	fmt.Fprintf(os.Stderr, "%d rapor, %d gerçek deprem yazıldı\n", len(entries), len(truths))
	return nil
}

// isc, ISC bülteninden gerçek bir dizinin AFAD ve Kandilli çözümlerini çekip
// katalog yazar (bkz. internal/replay/isc.go). Varsayılanlar 10 Ağustos 2025
// Sındırgı (Mw 6,1) depreminin ilk 7 günüdür. İstekler günlük parçalara
// bölünür: ISC tek seferde büyük aralıkta zaman aşımına düşüyor.
func isc(args []string) error {
	fs := flag.NewFlagSet("isc", flag.ExitOnError)
	out := fs.String("out", "-", "çıktı dosyası (- = stdout)")
	startS := fs.String("start", "2025-08-10T15:50:00Z", "başlangıç (UTC)")
	days := fs.Int("days", 7, "gün sayısı")
	bbox := fs.String("bbox", "38.7,39.7,27.5,28.9", "minlat,maxlat,minlon,maxlon")
	place := fs.String("place", "Sındırgı (Balıkesir)", "raporlara yazılacak yer adı")
	seed := fs.Uint64("seed", 1, "varış gecikmesi tohumu")
	base := fs.String("url", "http://www.isc.ac.uk/fdsnws/event/1/query", "ISC FDSN adresi")
	fs.Parse(args)

	start, err := time.Parse(time.RFC3339, *startS)
	if err != nil {
		return fmt.Errorf("-start: %w", err)
	}
	b := strings.Split(*bbox, ",")
	if len(b) != 4 {
		return errors.New("-bbox: minlat,maxlat,minlon,maxlon")
	}
	hc := &http.Client{Timeout: 3 * time.Minute}
	var obs []replay.Observation
	for d := 0; d < *days; d++ {
		from := start.Add(time.Duration(d) * 24 * time.Hour)
		q := url.Values{}
		q.Set("starttime", from.Format("2006-01-02T15:04:05"))
		q.Set("endtime", from.Add(24*time.Hour).Format("2006-01-02T15:04:05"))
		q.Set("minlatitude", b[0])
		q.Set("maxlatitude", b[1])
		q.Set("minlongitude", b[2])
		q.Set("maxlongitude", b[3])
		q.Set("includeallorigins", "true")
		q.Set("includeallmagnitudes", "true")
		req, err := http.NewRequest(http.MethodGet, *base+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", app.Env("ARTCI_USER_AGENT", source.UserAgent))
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			resp.Body.Close()
			return fmt.Errorf("isc: %s: HTTP %d", from.Format("2006-01-02"), resp.StatusCode)
		}
		o, err := replay.ParseISC(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s: %d çözüm\n", from.Format("2006-01-02"), len(o))
		obs = append(obs, o...)
	}

	entries := replay.CatalogFromObservations(obs, *place, *seed)
	var w io.Writer = os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
		if strings.HasSuffix(*out, ".gz") {
			zw := gzip.NewWriter(f)
			defer zw.Close() // f.Close'dan önce çalışır
			w = zw
		}
	}
	if err := replay.WriteCatalog(w, entries); err != nil {
		return err
	}
	truths, perSrc := map[string]bool{}, map[string]int{}
	for _, e := range entries {
		truths[e.Truth] = true
		perSrc[e.Report.Source]++
	}
	fmt.Fprintf(os.Stderr, "%d rapor (%v), %d ISC olayı yazıldı\n", len(entries), perSrc, len(truths))
	return nil
}

type result struct {
	RunID         string             `json:"run_id"`
	Reports       int                `json:"reports_published"`
	Speed         float64            `json:"speed"`
	PublishSecs   float64            `json:"publish_seconds"`
	DrainSecs     float64            `json:"drain_seconds"`
	ReportsPerSec float64            `json:"merger_reports_per_second"`
	Accuracy      replay.Accuracy    `json:"accuracy"`
	Latency       replay.Percentiles `json:"event_latency"`
	LatencyMissed int                `json:"events_without_update"`
	Unassigned    int                `json:"reports_without_event"`
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	in := fs.String("in", "", "katalog (JSONL; .gz uzantılıysa gzip)")
	speed := fs.Float64("speed", 10, "hızlandırma katsayısı; 0 = beklemeden")
	limit := fs.Int("limit", 0, "yalnızca ilk N rapor (0 = hepsi)")
	force := fs.Bool("force", false, "notifier grubu olsa da çalış")
	drainTimeout := fs.Duration("drain-timeout", 10*time.Minute, "merger'ın bitirmesi için üst sınır")
	jsonOut := fs.String("json", "", "sonuçları JSON olarak bu dosyaya da yaz")
	cleanup := fs.Bool("cleanup", false, "ölçümden sonra oynatılan rapor ve olayları sil")
	fs.Parse(args)
	if *in == "" {
		return errors.New("-in gerekli")
	}
	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	var r io.Reader = f
	if strings.HasSuffix(*in, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return fmt.Errorf("%s: %w", *in, err)
		}
		r = zr
	}
	entries, err := replay.ReadCatalog(r)
	f.Close()
	if err != nil {
		return err
	}
	if *limit > 0 && *limit < len(entries) {
		entries = entries[:*limit]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := app.LoadConfig("")
	rdb, err := app.Redis(ctx, cfg.RedisAddr)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer rdb.Close()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer st.Close()

	if err := guard(ctx, rdb, *force); err != nil {
		return err
	}

	runID := "rp" + strconv.FormatInt(time.Now().Unix(), 36)
	res := result{RunID: runID, Reports: len(entries), Speed: *speed}
	fmt.Printf("oynatma %s: %d rapor, hız %gx\n", runID, len(entries), *speed)

	// events.updated dinleyicisi: her olayın ilk görüldüğü an.
	seen := &seenSet{m: map[string]time.Time{}}
	startID, err := lastID(ctx, rdb, stream.EventsUpdated)
	if err != nil {
		return err
	}
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	var lwg sync.WaitGroup
	lwg.Go(func() { listen(lctx, rdb, startID, seen) })

	// Oynatma: oluş zamanları sabit bir kaymayla "şimdi"ye taşınır, aralıkları
	// korunur (eşleştirme pencereleri bozulmasın). Yalnızca varışlar hızlanır.
	t0 := entries[0].Report.OriginTime.Add(-time.Duration(entries[0].ArriveS * float64(time.Second)))
	start := time.Now()
	shift := start.Sub(t0)
	truthOf := map[string]string{}
	published := map[string]time.Time{}
	for i, e := range entries {
		if *speed > 0 {
			due := start.Add(time.Duration(e.ArriveS / *speed * float64(time.Second)))
			if d := time.Until(due); d > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(d):
				}
			}
		}
		r := e.Report
		r.SourceEventID = runID + "-" + r.SourceEventID
		r.OriginTime = r.OriginTime.Add(shift).UTC()
		r.ReceivedAt = time.Now().UTC()
		key := r.Source + "/" + r.SourceEventID
		if _, ok := published[key]; !ok {
			published[key] = r.ReceivedAt
		}
		truthOf[key] = e.Truth
		if _, err := stream.Publish(ctx, rdb, stream.ReportsRaw, r); err != nil {
			return fmt.Errorf("yayın: %w", err)
		}
		if (i+1)%500 == 0 {
			fmt.Printf("  %d/%d yayınlandı\n", i+1, len(entries))
		}
	}
	pubDone := time.Now()
	res.PublishSecs = pubDone.Sub(start).Seconds()

	if err := waitDrained(ctx, rdb, *drainTimeout); err != nil {
		return err
	}
	drained := time.Now()
	res.DrainSecs = drained.Sub(start).Seconds()
	res.ReportsPerSec = float64(len(entries)) / res.DrainSecs
	time.Sleep(time.Second) // son events.updated mesajları dinleyiciye ulaşsın
	lcancel()
	lwg.Wait()

	// Atamaları DB'den oku.
	rows, err := st.Pool.Query(ctx, `SELECT source, source_event_id, COALESCE(event_id::text, '')
		FROM reports WHERE source_event_id LIKE $1`, runID+"-%")
	if err != nil {
		return err
	}
	var as []replay.Assignment
	firstPub := map[string]time.Time{}
	eventIDs := map[string]bool{}
	for rows.Next() {
		var src, sid, ev string
		if err := rows.Scan(&src, &sid, &ev); err != nil {
			return err
		}
		key := src + "/" + sid
		if ev == "" {
			res.Unassigned++
			ev = "none:" + key
		} else {
			eventIDs[ev] = true
			if p, ok := published[key]; ok && (firstPub[ev].IsZero() || p.Before(firstPub[ev])) {
				firstPub[ev] = p
			}
		}
		as = append(as, replay.Assignment{Key: key, Truth: truthOf[key], Event: ev, Source: src})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	res.Accuracy = replay.Score(as)

	var lat []time.Duration
	type slow struct {
		ev  string
		d   time.Duration
		pub time.Time
	}
	var slowest []slow
	for ev, p := range firstPub {
		if t, ok := seen.get(ev); ok {
			lat = append(lat, t.Sub(p))
			slowest = append(slowest, slow{ev, t.Sub(p), p})
		} else {
			res.LatencyMissed++
		}
	}
	res.Latency = replay.Summarize(lat)
	report(res)
	sort.Slice(slowest, func(i, j int) bool { return slowest[i].d > slowest[j].d })
	for i, s := range slowest {
		if i == 3 {
			break
		}
		fmt.Printf("  yavaş: %s %v (ilk rapor oynatmanın %.1f. saniyesinde)\n", s.ev, s.d.Round(time.Millisecond), s.pub.Sub(start).Seconds())
	}

	if *jsonOut != "" {
		b, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			return err
		}
	}
	if *cleanup {
		ids := make([]string, 0, len(eventIDs))
		for id := range eventIDs {
			ids = append(ids, id)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM reports WHERE source_event_id LIKE $1`, runID+"-%"); err != nil {
			return err
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM events e WHERE e.id = ANY($1::uuid[])
			AND NOT EXISTS (SELECT 1 FROM reports r WHERE r.event_id = e.id)`, ids); err != nil {
			return err
		}
		fmt.Println("oynatılan rapor ve olaylar silindi")
	}
	return nil
}

func report(r result) {
	a := r.Accuracy
	fmt.Println()
	fmt.Println("== Sonuç ==")
	fmt.Printf("raporlar           %d (ayrı: %d, olaysız: %d)\n", r.Reports, a.Reports, r.Unassigned)
	fmt.Printf("yayın / bitiş      %.1f sn / %.1f sn\n", r.PublishSecs, r.DrainSecs)
	fmt.Printf("merger iş hacmi    %.1f rapor/sn", r.ReportsPerSec)
	if r.Speed > 0 {
		fmt.Printf("  (hız %gx: oynatma takvimiyle sınırlı)", r.Speed)
	}
	fmt.Println()
	fmt.Printf("gerçek / oluşan    %d deprem / %d olay\n", a.TrueEvents, a.MergedEvents)
	fmt.Printf("precision          %.4f  (birleştirilen çiftlerin doğru oranı)\n", a.Precision)
	fmt.Printf("recall             %.4f  (aynı depremin çiftlerinden birleştirilenler)\n", a.Recall)
	fmt.Printf("F1                 %.4f\n", a.F1)
	fmt.Printf("tam doğru deprem   %.2f%%\n", 100*a.ExactlyCorrect)
	l := r.Latency
	fmt.Printf("olay gecikmesi     p50 %v · p95 %v · p99 %v · max %v (n=%d, eksik %d)\n",
		l.P50.Round(time.Millisecond), l.P95.Round(time.Millisecond), l.P99.Round(time.Millisecond),
		l.Max.Round(time.Millisecond), l.N, r.LatencyMissed)
}

// guard, gerçek abonelere bildirim gitmesini önler.
func guard(ctx context.Context, rdb *redis.Client, force bool) error {
	groups, err := rdb.XInfoGroups(ctx, stream.EventsUpdated).Result()
	if err != nil && !strings.Contains(err.Error(), "no such key") {
		return err
	}
	for _, g := range groups {
		if g.Name == "notifier" && !force {
			return errors.New(`events.updated üzerinde "notifier" grubu var: oynatılan depremler abonelere ` +
				`bildirim olarak gidebilir. Ayrı bir ortam kullanın ya da -force verin`)
		}
	}
	return nil
}

func lastID(ctx context.Context, rdb *redis.Client, s string) (string, error) {
	msgs, err := rdb.XRevRangeN(ctx, s, "+", "-", 1).Result()
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "0", nil
	}
	return msgs[0].ID, nil
}

type seenSet struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (s *seenSet) add(id string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		s.m[id] = t
	}
}

func (s *seenSet) get(id string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[id]
	return t, ok
}

// listen, events.updated'ı grup olmadan okur (ölçüm yan etkisiz olsun).
func listen(ctx context.Context, rdb *redis.Client, from string, seen *seenSet) {
	id := from
	for ctx.Err() == nil {
		res, err := rdb.XRead(ctx, &redis.XReadArgs{Streams: []string{stream.EventsUpdated, id}, Count: 500, Block: 500 * time.Millisecond}).Result()
		if err != nil {
			continue
		}
		now := time.Now()
		for _, s := range res {
			for _, m := range s.Messages {
				id = m.ID
				data, _ := m.Values["data"].(string)
				var u normalize.EventUpdate
				if json.Unmarshal([]byte(data), &u) == nil && u.Event.ID != "" {
					seen.add(u.Event.ID, now)
				}
			}
		}
	}
}

// waitDrained, merger grubunun bekleyen ve okunmamış mesajı kalmayana dek bekler.
func waitDrained(ctx context.Context, rdb *redis.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		groups, err := rdb.XInfoGroups(ctx, stream.ReportsRaw).Result()
		if err != nil {
			return err
		}
		found := false
		for _, g := range groups {
			if g.Name != "merger" {
				continue
			}
			found = true
			if g.Pending == 0 && g.Lag == 0 {
				return nil
			}
		}
		if !found {
			return errors.New(`reports.raw üzerinde "merger" grubu yok: merger çalışıyor mu?`)
		}
		if time.Now().After(deadline) {
			return errors.New("merger zamanında bitiremedi")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
