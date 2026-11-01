// Package source, üç veri kaynağının (AFAD, Kandilli, EMSC) istemcilerini ve
// ayrıştırıcılarını içerir (Plan §5, §9).
//
// Katmanlar:
//   - Ayrıştırıcılar (ParseAFAD, ParseKandilli, ParseEMSC*) saf fonksiyonlardır;
//     ağ görmez, testdata/source altındaki örneklerle test edilir.
//   - Fetcher'lar HTTP ile ham yanıtı alıp ayrıştırıcıya verir.
//   - Poller, bir Fetcher'ı backoff + jitter ve circuit breaker ile döngüde çalıştırır.
//   - EMSCStream, SeismicPortal WebSocket akışını dinler, kopuklukta FDSN ile boşluğu doldurur.
//
// Tek bir bozuk kayıt hiçbir zaman tüm yanıtı düşürmez: BadRecord olarak döner
// ve çağıran onu reports.dlq akışına yazar.
package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nisaguven/artci/internal/normalize"
)

// UserAgent, tüm isteklerde gönderilir (Plan §5: açık User-Agent + iletişim).
// ARTCI_USER_AGENT ile değiştirilebilir.
const UserAgent = "artci/0.1 (egitim projesi; +https://github.com/Nisagwn/artci)"

// BadRecord, ayrıştırılamayan veya doğrulanamayan tek bir kayıttır.
type BadRecord struct {
	Raw    string
	Reason string
}

// Result, bir kaynaktan tek okumanın sonucudur.
type Result struct {
	Reports []normalize.Report
	Bad     []BadRecord
}

// Fetcher, bir kaynaktan son kayıtları okur.
type Fetcher interface {
	Name() string
	Fetch(ctx context.Context) (Result, error)
}

// Sink, toplayıcının çıktısını alır (üretimde Redis Streams).
type Sink interface {
	Publish(ctx context.Context, r normalize.Report) error
	DLQ(ctx context.Context, source string, b BadRecord) error
}

// maxBody: tek yanıt için üst sınır; kaynak bozulup dev bir sayfa dönerse belleği korur.
const maxBody = 8 << 20

// HTTPClient, kaynak isteklerinde kullanılan ortak istemcidir.
type HTTPClient struct {
	Client    *http.Client
	UserAgent string
}

// NewHTTPClient, makul zaman aşımlı bir istemci döner.
func NewHTTPClient(ua string) *HTTPClient {
	if ua == "" {
		ua = UserAgent
	}
	return &HTTPClient{Client: &http.Client{Timeout: 20 * time.Second}, UserAgent: ua}
}

// ErrStatus, 2xx olmayan yanıttır.
type ErrStatus struct {
	URL  string
	Code int
}

func (e *ErrStatus) Error() string { return fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Code) }

// Get, gövdeyi okur. 2xx dışı yanıtlar hata sayılır.
func (c *HTTPClient) Get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &ErrStatus{URL: url, Code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, errors.New("yanıt çok büyük")
	}
	return b, nil
}

// finish, doğrulamadan geçemeyen raporları BadRecord'a taşır.
func finish(reports []normalize.Report, bad []BadRecord) Result {
	out := reports[:0]
	for _, r := range reports {
		if err := r.Validate(); err != nil {
			bad = append(bad, BadRecord{Raw: string(r.Raw), Reason: "validate: " + err.Error()})
			continue
		}
		out = append(out, r)
	}
	return Result{Reports: out, Bad: bad}
}
