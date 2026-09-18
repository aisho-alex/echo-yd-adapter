package chatllm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func llmServer(t *testing.T, handler http.HandlerFunc) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := New(Config{
		BaseURL: srv.URL,
		APIKey:  "sk-test",
		Model:   "gpt-oss-120b",
		System:  "ты ассистент",
		Timeout: 5 * time.Second,
		Retries: 2,
	})
	c.RetryPause = time.Millisecond
	return c, srv.Close
}

func TestCompleteSendsSystemContextUser(t *testing.T) {
	var gotAuth, gotModel string
	var gotMsgs []Msg
	c, closeSrv := llmServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model    string `json:"model"`
			Messages []Msg  `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		gotMsgs = body.Messages
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "ответ модели"}},
			},
		})
	})
	defer closeSrv()

	out, err := c.Complete([]Msg{
		{Role: "system", Content: "ты ассистент"},
		{Role: "user", Content: "привет"},
		{Role: "assistant", Content: "привет!"},
		{Role: "user", Content: "как дела?"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "ответ модели" {
		t.Fatalf("out = %q", out)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotModel != "gpt-oss-120b" {
		t.Fatalf("model = %q", gotModel)
	}
	if len(gotMsgs) != 4 || gotMsgs[0].Role != "system" || gotMsgs[3].Content != "как дела?" {
		t.Fatalf("messages = %+v", gotMsgs)
	}
}

func TestCompleteRetries429ThenSucceeds(t *testing.T) {
	var calls int
	c, closeSrv := llmServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ок"}}},
		})
	})
	defer closeSrv()

	out, err := c.Complete([]Msg{{Role: "user", Content: "hi"}})
	if err != nil || out != "ок" {
		t.Fatalf("Complete = %q, %v; want ок, nil", out, err)
	}
	if calls != 3 {
		t.Fatalf("вызовов %d, want 3", calls)
	}
}

func TestCompleteFailsAfterRetries(t *testing.T) {
	var calls int
	c, closeSrv := llmServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer closeSrv()

	_, err := c.Complete([]Msg{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want HTTP 500", err)
	}
	if calls != 3 {
		t.Fatalf("вызовов %d, want 3 (1 + 2 ретрая)", calls)
	}
}

func TestAnswerAssemblesSystemAndClampsContext(t *testing.T) {
	var gotMsgs []Msg
	c, closeSrv := llmServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Msg `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotMsgs = body.Messages
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ок"}}},
		})
	})
	defer closeSrv()

	history := []Msg{
		{Role: "user", Content: "1"}, {Role: "assistant", Content: "2"},
		{Role: "user", Content: "3"}, {Role: "assistant", Content: "4"},
		{Role: "user", Content: "5"},
	}
	if _, err := c.Answer(history, "шесть"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	// system + последние 3 реплики (3,4,5) + текущий текст
	if len(gotMsgs) != 5 {
		t.Fatalf("messages = %d, want 5: %+v", len(gotMsgs), gotMsgs)
	}
	if gotMsgs[0].Role != "system" {
		t.Fatalf("первым должен быть system: %+v", gotMsgs)
	}
	if gotMsgs[1].Content != "3" || gotMsgs[3].Content != "5" {
		t.Fatalf("контекст не обрезан до последних 3: %+v", gotMsgs)
	}
	if gotMsgs[4].Content != "шесть" || gotMsgs[4].Role != "user" {
		t.Fatalf("текст пользователя потерян: %+v", gotMsgs)
	}
}

func TestCompleteEmptyAnswerIsError(t *testing.T) {
	c, closeSrv := llmServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "  "}}},
		})
	})
	defer closeSrv()

	_, err := c.Complete([]Msg{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "пустой ответ") {
		t.Fatalf("err = %v, want пустой ответ", err)
	}
}
