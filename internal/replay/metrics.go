package replay

import (
	"math"
	"sort"
	"time"
)

// Accuracy, birleştirme doğruluğudur.
//
// Çift tabanlı ölçüm: aynı kaynaktan olmayan her rapor çifti için
// "aynı gerçek deprem mi?" ile "merger aynı olaya mı bağladı?" karşılaştırılır.
//   - Precision: birleştirilen çiftlerin ne kadarı gerçekten aynı deprem
//     (düşükse: farklı depremler yanlışlıkla birleşiyor — artçı dizisinin riski).
//   - Recall: aynı depremin çiftlerinin ne kadarı birleştirildi
//     (düşükse: aynı deprem birden çok olay olarak görünüyor — mükerrer bildirim).
//
// Çiftler kaynak kimliği (source_event_id) başına bir kez sayılır; revizyonlar
// aynı rapor satırını güncellediği için ayrıca sayılmaz.
type Accuracy struct {
	Reports        int     // ayrı rapor (source, source_event_id)
	TrueEvents     int     // en az bir raporu olan gerçek deprem
	MergedEvents   int     // merger'ın oluşturduğu olay
	Precision      float64 // çift tabanlı
	Recall         float64
	F1             float64
	ExactlyCorrect float64 // raporları tek ve yalnızca kendisine ait bir olayda toplanan gerçek depremlerin oranı
}

// Assignment: rapor anahtarı -> (gerçek deprem, merger olayı).
type Assignment struct {
	Key, Truth, Event, Source string
}

// Score, atamalardan doğruluğu hesaplar.
func Score(as []Assignment) Accuracy {
	var acc Accuracy
	acc.Reports = len(as)
	truths := map[string][]int{}
	events := map[string][]int{}
	for i, a := range as {
		truths[a.Truth] = append(truths[a.Truth], i)
		events[a.Event] = append(events[a.Event], i)
	}
	acc.TrueEvents, acc.MergedEvents = len(truths), len(events)

	// Çift sayımı: gerçek kümeler ve olay kümeleri üzerinden; O(n·k).
	var tp, sameTruth, sameEvent float64
	for _, idx := range truths {
		sameTruth += crossPairs(as, idx)
	}
	for _, idx := range events {
		sameEvent += crossPairs(as, idx)
		byTruth := map[string][]int{}
		for _, i := range idx {
			byTruth[as[i].Truth] = append(byTruth[as[i].Truth], i)
		}
		for _, sub := range byTruth {
			tp += crossPairs(as, sub)
		}
	}
	acc.Precision, acc.Recall = ratio(tp, sameEvent), ratio(tp, sameTruth)
	if acc.Precision+acc.Recall > 0 {
		acc.F1 = 2 * acc.Precision * acc.Recall / (acc.Precision + acc.Recall)
	}

	exact := 0
	for _, idx := range truths {
		ev := as[idx[0]].Event
		ok := true
		for _, i := range idx {
			if as[i].Event != ev {
				ok = false
				break
			}
		}
		if ok && len(events[ev]) == len(idx) {
			exact++
		}
	}
	acc.ExactlyCorrect = ratio(float64(exact), float64(len(truths)))
	return acc
}

// crossPairs: kümedeki farklı kaynaklı rapor çiftlerinin sayısı.
// Aynı kaynaktan iki rapor hiçbir zaman aynı olay olmamalıdır (merger kuralı);
// bu yüzden doğruluk yalnızca kaynaklar arası çiftlerle ölçülür.
func crossPairs(as []Assignment, idx []int) float64 {
	bySource := map[string]float64{}
	for _, i := range idx {
		bySource[as[i].Source]++
	}
	n := float64(len(idx))
	pairs := n * (n - 1) / 2
	for _, k := range bySource {
		pairs -= k * (k - 1) / 2
	}
	return pairs
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 1
	}
	return a / b
}

// Percentiles, gecikme dağılımının özetidir.
type Percentiles struct {
	N             int
	P50, P95, P99 time.Duration
	Max           time.Duration
}

// Summarize, örneklerden yüzdelikleri hesaplar (en yakın sıra yöntemi).
func Summarize(samples []time.Duration) Percentiles {
	if len(samples) == 0 {
		return Percentiles{}
	}
	s := append([]time.Duration(nil), samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) time.Duration {
		i := int(math.Ceil(p*float64(len(s)))) - 1
		if i < 0 {
			i = 0
		}
		return s[i]
	}
	return Percentiles{N: len(s), P50: at(0.50), P95: at(0.95), P99: at(0.99), Max: s[len(s)-1]}
}
