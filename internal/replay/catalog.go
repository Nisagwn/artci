// Package replay, bir deprem dizisini hatta hızlandırarak yeniden oynatmak ve
// sonucu ölçmek için gereken saf parçalardır (Plan §13).
//
// Katalog biçimi (JSONL, satır başına bir rapor):
//
//	{"truth":"T0042","arrive_s":131.5,"report":{...normalize.Report...}}
//
// truth: raporun ait olduğu gerçek deprem. Birleştirme doğruluğu, merger'ın
// verdiği olay kimlikleri bu etiketle karşılaştırılarak ölçülür.
// arrive_s: raporun hatta ulaşacağı an (dizinin ilk oluş zamanından saniye).
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/nisaguven/artci/internal/normalize"
)

// Entry, katalogdaki tek rapordur.
type Entry struct {
	Truth   string           `json:"truth"`
	ArriveS float64          `json:"arrive_s"`
	Report  normalize.Report `json:"report"`
}

// ReadCatalog, JSONL kataloğu okur ve varış sırasına dizer.
func ReadCatalog(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("satır %d: %w", line, err)
		}
		if e.Truth == "" {
			return nil, fmt.Errorf("satır %d: truth boş", line)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ArriveS < out[j].ArriveS })
	return out, nil
}

// WriteCatalog, kataloğu JSONL olarak yazar.
func WriteCatalog(w io.Writer, entries []Entry) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return bw.Flush()
}
