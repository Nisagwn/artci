package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTelegramURL, Bot API adresidir; testlerde httptest sunucusu verilir.
const DefaultTelegramURL = "https://api.telegram.org"

// Telegram, Bot API'nin kullandığımız küçük alt kümesidir (harici kütüphane yok:
// iki uç için bağımlılık eklemek, öğrenme hedefine de ters).
type Telegram struct {
	BaseURL    string
	Token      string
	HTTP       *http.Client
	MaxRetries int // 429 için en fazla yeniden deneme (varsayılan 3)
	// Sleep, 429 beklemesi içindir; testlerde gerçek bekleme olmasın diye değiştirilir.
	Sleep func(ctx context.Context, d time.Duration) error
	// Keyboard doluysa her mesaja kalıcı düğmeler olarak eklenir (ör. MenuKeyboard).
	Keyboard [][]string
}

// NewTelegram, varsayılanlarla bir istemci döner.
func NewTelegram(baseURL, token string) *Telegram {
	if baseURL == "" {
		baseURL = DefaultTelegramURL
	}
	return &Telegram{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		// Long polling 25 sn sürer; istemci zaman aşımı bundan uzun olmalı.
		HTTP:       &http.Client{Timeout: 40 * time.Second},
		MaxRetries: 3,
		Sleep:      sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// APIError, Telegram'ın ok=false yanıtıdır.
type APIError struct {
	Code        int
	Description string
	RetryAfter  int // saniye; yalnızca 429'da dolu
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %d: %s", e.Code, e.Description)
}

// IsPermanent: yeniden denemenin işe yaramayacağı hatalar (ör. 403 kullanıcı botu
// engelledi, 400 sohbet yok). Bunlar için bildirim günlüğü geri alınmaz; aksi
// halde aynı mesaj sonsuza dek yeniden denenirdi.
func IsPermanent(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == http.StatusBadRequest || ae.Code == http.StatusForbidden
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call, bir Bot API metodunu JSON gövdeyle çağırır; 429'da retry_after kadar bekleyip yeniden dener.
func (t *Telegram) call(ctx context.Context, method string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := t.do(ctx, method, body, out)
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == http.StatusTooManyRequests && attempt < t.MaxRetries {
			wait := time.Duration(ae.RetryAfter) * time.Second
			if wait <= 0 {
				wait = time.Second
			}
			if serr := t.Sleep(ctx, wait); serr != nil {
				return serr
			}
			continue
		}
		return err
	}
}

func (t *Telegram) do(ctx context.Context, method string, body []byte, out any) error {
	url := t.BaseURL + "/bot" + t.Token + "/" + method
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return t.redact(err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := t.HTTP.Do(hreq)
	if err != nil {
		return t.redact(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return t.redact(err)
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d, çözülemeyen yanıt", method, resp.StatusCode)
	}
	if !ar.OK {
		code := ar.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &APIError{Code: code, Description: ar.Description, RetryAfter: ar.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

// redact: net/http hataları URL'yi, dolayısıyla bot token'ını içerir; loglara sızmasın.
func (t *Telegram) redact(err error) error {
	if t.Token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.Token, "<token>"))
}

// Update, getUpdates sonucundaki tek güncellemedir (yalnızca kullandığımız alanlar).
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Message, gelen bir sohbet mesajıdır.
type Message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
}

// GetUpdates, offset'ten itibaren güncellemeleri long polling ile alır.
// offset = son işlenen update_id + 1; Telegram öncekileri bu sayede onaylanmış sayar.
func (t *Telegram) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	var ups []Update
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message"},
	}, &ups)
	return ups, err
}

// SendMessage, HTML parse mode ile mesaj gönderir. Metin çağıran tarafından
// EscapeHTML ile hazırlanmış olmalıdır.
func (t *Telegram) SendMessage(ctx context.Context, chatID int64, html string) error {
	req := map[string]any{
		"chat_id":                  chatID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if len(t.Keyboard) > 0 {
		rows := make([][]map[string]string, len(t.Keyboard))
		for i, row := range t.Keyboard {
			for _, label := range row {
				rows[i] = append(rows[i], map[string]string{"text": label})
			}
		}
		req["reply_markup"] = map[string]any{
			"keyboard":                rows,
			"resize_keyboard":         true,
			"is_persistent":           true,
			"input_field_placeholder": "İl adı yazın, ör. İzmir",
		}
	}
	return t.call(ctx, "sendMessage", req, nil)
}
