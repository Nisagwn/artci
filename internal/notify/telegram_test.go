package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTelegram, Bot API'yi taklit eder; gerçek Telegram'a istek atılmaz.
func fakeTelegram(t *testing.T, h func(method string, body map[string]any) (int, string)) *Telegram {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/botTEST:TOKEN/") {
			t.Errorf("beklenmeyen yol %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		code, resp := h(strings.TrimPrefix(r.URL.Path, "/botTEST:TOKEN/"), body)
		w.WriteHeader(code)
		w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	tg := NewTelegram(srv.URL, "TEST:TOKEN")
	tg.Sleep = func(context.Context, time.Duration) error { return nil }
	return tg
}

func TestSendMessage(t *testing.T) {
	tg := fakeTelegram(t, func(method string, body map[string]any) (int, string) {
		if method != "sendMessage" || body["parse_mode"] != "HTML" || body["chat_id"].(float64) != 42 || body["text"] != "<b>a</b>" {
			t.Errorf("beklenmeyen istek %s %v", method, body)
		}
		return 200, `{"ok":true,"result":{"message_id":1}}`
	})
	if err := tg.SendMessage(context.Background(), 42, "<b>a</b>"); err != nil {
		t.Fatal(err)
	}
}

func TestSendMessageKeyboard(t *testing.T) {
	var markup any
	tg := fakeTelegram(t, func(_ string, body map[string]any) (int, string) {
		markup = body["reply_markup"]
		return 200, `{"ok":true,"result":{}}`
	})
	if err := tg.SendMessage(context.Background(), 1, "x"); err != nil || markup != nil {
		t.Fatalf("düğme verilmediyse reply_markup olmamalı: %v %v", markup, err)
	}
	tg.Keyboard = MenuKeyboard
	if err := tg.SendMessage(context.Background(), 1, "x"); err != nil {
		t.Fatal(err)
	}
	m, _ := markup.(map[string]any)
	rows, _ := m["keyboard"].([]any)
	if len(rows) != 2 || m["is_persistent"] != true {
		t.Fatalf("reply_markup = %v", markup)
	}
	first := rows[0].([]any)[0].(map[string]any)
	if first["text"] != BtnSubscriptions {
		t.Fatalf("ilk düğme = %v", first)
	}
}

func TestSendMessageRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var waited []time.Duration
	tg := fakeTelegram(t, func(string, map[string]any) (int, string) {
		if calls.Add(1) < 3 {
			return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`
		}
		return 200, `{"ok":true,"result":{}}`
	})
	tg.Sleep = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	if err := tg.SendMessage(context.Background(), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(waited) != 2 || waited[0] != 7*time.Second {
		t.Fatalf("calls=%d waited=%v", calls.Load(), waited)
	}
}

func TestSendMessageRetryExhausted(t *testing.T) {
	var calls atomic.Int32
	tg := fakeTelegram(t, func(string, map[string]any) (int, string) {
		calls.Add(1)
		return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`
	})
	err := tg.SendMessage(context.Background(), 1, "x")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 429 || IsPermanent(err) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != int32(tg.MaxRetries+1) {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestSendMessagePermanent(t *testing.T) {
	tg := fakeTelegram(t, func(string, map[string]any) (int, string) {
		return 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`
	})
	if err := tg.SendMessage(context.Background(), 1, "x"); !IsPermanent(err) {
		t.Fatalf("kalıcı hata bekleniyordu: %v", err)
	}
}

func TestGetUpdates(t *testing.T) {
	tg := fakeTelegram(t, func(method string, body map[string]any) (int, string) {
		if method != "getUpdates" || body["offset"].(float64) != 10 || body["timeout"].(float64) != 25 {
			t.Errorf("beklenmeyen istek %s %v", method, body)
		}
		return 200, `{"ok":true,"result":[{"update_id":10,"message":{"message_id":5,"text":"/start","chat":{"id":99,"type":"private"}}},{"update_id":11}]}`
	})
	ups, err := tg.GetUpdates(context.Background(), 10, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].Message.Chat.ID != 99 || ups[0].Message.Text != "/start" || ups[1].Message != nil {
		t.Fatalf("ups = %+v", ups)
	}
}

func TestTokenRedacted(t *testing.T) {
	tg := NewTelegram("http://127.0.0.1:1", "SECRET:TOKEN") // bağlantı reddedilir
	tg.HTTP.Timeout = 2 * time.Second
	err := tg.SendMessage(context.Background(), 1, "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("token hatada görünmemeli: %v", err)
	}
}
