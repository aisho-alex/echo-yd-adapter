package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ntfyMsg — контракт JSON-публикации ntfy: priority обязан быть числом,
// строка даёт HTTP 400 (проверено на живом ntfy.sh 21.09.2026).
type ntfyMsg struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title"`
	Message  string   `json:"message"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags"`
	Click    string   `json:"click"`
}

// fakeNtfy поднимает сервер, разбирает уведомления строго по контракту.
func fakeNtfy(t *testing.T, status int) (*Client, *[]ntfyMsg, *[]string) {
	t.Helper()
	var got []ntfyMsg
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var m ntfyMsg
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, `{"error":"invalid request: request body must be valid JSON"}`, 400)
			return
		}
		got = append(got, m)
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "echo-test-topic")
	c.RetryPause = time.Millisecond
	return c, &got, &paths
}

func TestReplySendsFactAndClick(t *testing.T) {
	c, got, paths := fakeNtfy(t, http.StatusOK)
	if err := c.Reply("20260918T150233Z-a1b2"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("уведомлений %d, want 1", len(*got))
	}
	// Publish as JSON: POST в корень, иначе ntfy покажет JSON сырым текстом
	if (*paths)[0] != "/" {
		t.Fatalf("path = %s, want /", (*paths)[0])
	}
	m := (*got)[0]
	if m.Topic != "echo-test-topic" {
		t.Fatalf("topic = %v", m.Topic)
	}
	if m.Message != "Новый ответ" {
		t.Fatalf("message = %v — текст ответа утёк?", m.Message)
	}
	if m.Priority != 4 {
		t.Fatalf("priority = %d, want 4 (число)", m.Priority)
	}
	if m.Click != "echopult://open?ref=20260918T150233Z-a1b2" {
		t.Fatalf("click = %v", m.Click)
	}
}

func TestReplyRetriesOnServerFailure(t *testing.T) {
	c, got, _ := fakeNtfy(t, http.StatusInternalServerError)
	if err := c.Reply("id1"); err == nil {
		t.Fatal("ожидалась ошибка после ретрая")
	}
	if len(*got) != 2 {
		t.Fatalf("попыток %d, want 2", len(*got))
	}
}
func TestReplyFailsOnUnreachableServer(t *testing.T) {
	c := New("http://127.0.0.1:1", "echo-test-topic") // порт 1 — никто не слушает
	c.RetryPause = time.Millisecond
	if err := c.Reply("id1"); err == nil {
		t.Fatal("ожидалась сетевая ошибка")
	}
}
