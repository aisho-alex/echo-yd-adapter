package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yd-adapter/internal/notify"
)

// fakeNtfy поднимает ntfy-сервер; уведомления копятся в слайсе.
func fakeNtfy(t *testing.T, status int) *notify.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := notify.New(srv.URL, "echo-test-topic")
	c.RetryPause = time.Millisecond
	return c
}

func TestPushOutNotifies(t *testing.T) {
	f, p, st, q := setup(t)
	p.Ntfy = fakeNtfy(t, http.StatusOK)
	seedTask(t, f, msgID, false)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	writeOutbox(t, q, msgID, "готово")

	if n, err := p.PushOut(); err != nil || n != 1 {
		t.Fatalf("PushOut = %d, %v; want 1, nil", n, err)
	}
	env := outEnvelope(t, f)
	if env["ref"] != msgID {
		t.Fatalf("env = %v", env)
	}
}

func TestChatReplyNotifies(t *testing.T) {
	f, p, st, _ := setup(t)
	p.Ntfy = fakeNtfy(t, http.StatusOK)
	p.Chat, _ = fakeLLM(t, "ответ", http.StatusOK)

	seedChat(t, f, msgID, "привет", false, nil)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	if outEnvelope(t, f)["ref"] != msgID {
		t.Fatal("ответ не опубликован")
	}
}

func TestNtfyDownDoesNotBreakPublishing(t *testing.T) {
	f, p, st, q := setup(t)
	// сервер отвечает 500: ретраи внутри notify исчерпаются, публикация — нет
	p.Ntfy = fakeNtfy(t, http.StatusInternalServerError)
	seedTask(t, f, msgID, false)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	writeOutbox(t, q, msgID, "готово")

	if n, err := p.PushOut(); err != nil || n != 1 {
		t.Fatalf("PushOut = %d, %v; want 1, nil — сбой push не должен валить цикл", n, err)
	}
	if outEnvelope(t, f)["ref"] != msgID {
		t.Fatal("конверт не опубликован из-за упавшего ntfy")
	}
}

func TestNilNtfyMeansSilence(t *testing.T) {
	f, p, st, q := setup(t) // p.Ntfy == nil — push выключен
	seedTask(t, f, msgID, false)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	writeOutbox(t, q, msgID, "готово")
	if n, err := p.PushOut(); err != nil || n != 1 {
		t.Fatalf("PushOut = %d, %v", n, err)
	}
}

func writeOutbox(t *testing.T, queueDir, id, text string) {
	t.Helper()
	body, _ := json.Marshal(OutResult{
		ID: id, ChatID: 0, OK: true, Worker: "laptop", Text: text,
	})
	dst := filepath.Join(queueDir, "outbox", id+".json")
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatalf("outbox: %v", err)
	}
}
