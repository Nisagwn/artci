package replay

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

// ISC bülteni (International Seismological Centre) her deprem için tek bir
// olay kaydı tutar; altında kurumların kendi çözümleri (origin) ayrı ayrı
// durur. Gerçek bir dizide tekilleştirmeyi ölçmek için bu yapı tam aradığımız
// şeydir: kurum çözümleri birer rapor, ISC olay kimliği de doğru cevap
// (truth) olur. Doğru cevap ISC'nin gruplamasıdır; incelenmemiş dönemlerde
// bu da otomatik bir karardır, elle doğrulanmış değildir.

// iscAgencies, ISC kurum kodlarını hattaki kaynaklara eşler. EMSC çözümleri
// ISC bülteninde yer almadığı için gerçek katalog iki kaynaklıdır.
var iscAgencies = map[string]string{
	"AFAD": normalize.SourceAFAD,
	"ISK":  normalize.SourceKandilli,
}

// Observation, bir kurumun tek bir depreme ait çözümüdür.
type Observation struct {
	Truth  string
	Report normalize.Report
}

type qmlValue struct {
	Value float64 `xml:"value"`
}

type qmlDoc struct {
	Events []struct {
		ID      string `xml:"publicID,attr"`
		Origins []struct {
			ID   string `xml:"publicID,attr"`
			Time struct {
				Value string `xml:"value"`
			} `xml:"time"`
			Lat    qmlValue  `xml:"latitude"`
			Lon    qmlValue  `xml:"longitude"`
			Depth  *qmlValue `xml:"depth"`
			Agency string    `xml:"creationInfo>agencyID"`
		} `xml:"origin"`
		Magnitudes []struct {
			Mag      qmlValue `xml:"mag"`
			Type     string   `xml:"type"`
			OriginID string   `xml:"originID"`
		} `xml:"magnitude"`
	} `xml:"eventParameters>event"`
}

// magRank: Kandilli ayrıştırıcısıyla aynı tercih (Mw > ML > MD), sonra diğerleri.
func magRank(t string) int {
	switch strings.ToLower(t) {
	case "mw":
		return 0
	case "ml":
		return 1
	case "md":
		return 2
	}
	return 3
}

// ParseISC, ISC FDSN QuakeML yanıtını (includeallorigins=true,
// includeallmagnitudes=true) gözlemlere çevirir. Yalnızca eşlenen kurumların
// büyüklüğü olan çözümleri alınır; bir olayda aynı kurumun birden çok çözümü
// varsa ilki kullanılır (canlıda kaynak tek rapor yayınlar, revizyon upsert'tir).
func ParseISC(r io.Reader) ([]Observation, error) {
	var doc qmlDoc
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		if err == io.EOF { // 204: dönemde olay yok
			return nil, nil
		}
		return nil, fmt.Errorf("isc: quakeml: %w", err)
	}
	var out []Observation
	for _, ev := range doc.Events {
		truth := strings.TrimPrefix(ev.ID, "smi:ISC/evid=")
		used := map[string]bool{}
		for _, o := range ev.Origins {
			src, ok := iscAgencies[o.Agency]
			if !ok || used[src] {
				continue
			}
			mag, magType, rank := 0.0, "", 99
			for _, m := range ev.Magnitudes {
				if m.OriginID == o.ID && magRank(m.Type) < rank {
					mag, magType, rank = m.Mag.Value, m.Type, magRank(m.Type)
				}
			}
			if rank == 99 {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, o.Time.Value)
			if err != nil {
				return nil, fmt.Errorf("isc: %s: zaman %q: %w", o.ID, o.Time.Value, err)
			}
			rep := normalize.Report{
				Source:        src,
				SourceEventID: strings.TrimPrefix(o.ID, "smi:ISC/origid="),
				OriginTime:    t.UTC(),
				Lat:           o.Lat.Value,
				Lon:           o.Lon.Value,
				Magnitude:     mag,
				MagType:       magType,
			}
			if o.Depth != nil {
				d := math.Round(o.Depth.Value/100) / 10 // metre -> km, 1 ondalık
				rep.DepthKm = &d
			}
			used[src] = true
			out = append(out, Observation{Truth: truth, Report: rep})
		}
	}
	return out, nil
}

// CatalogFromObservations, gözlemlere varış anı ekleyip katalog yapar.
// Bültende kaynakların yayın gecikmesi yoktur; gecikme sentetik üreticideki
// kaynak modelinden (aynı dağılım ve ilk dakikalardaki yığılma) tohumla
// örneklenir. Böylece raporların merger'a geliş sırası gerçekçi olur ama
// konum, zaman ve büyüklük farkları tamamen gerçektir.
func CatalogFromObservations(obs []Observation, placeName string, seed uint64) []Entry {
	if len(obs) == 0 {
		return nil
	}
	sorted := append([]Observation(nil), obs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Report.OriginTime.Before(sorted[j].Report.OriginTime)
	})
	start := sorted[0].Report.OriginTime
	models := map[string]sourceModel{}
	for _, sm := range sourceModels {
		models[sm.name] = sm
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	out := make([]Entry, 0, len(sorted))
	for _, o := range sorted {
		sm := models[o.Report.Source]
		since := o.Report.OriginTime.Sub(start)
		burst := 1 + sm.stalledAfterBurstScale*math.Exp(-since.Minutes()/30)
		delay := (sm.delayMin + rng.Float64()*(sm.delayMax-sm.delayMin)) * burst
		r := o.Report
		r.PlaceName = placeName
		r.Raw, _ = json.Marshal(map[string]any{"isc_evid": o.Truth, "isc_origid": r.SourceEventID})
		out = append(out, Entry{Truth: o.Truth, ArriveS: since.Seconds() + delay, Report: r})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ArriveS < out[j].ArriveS })
	return out
}
