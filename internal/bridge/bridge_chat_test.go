package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yd-adapter/internal/chatllm"
	"yd-adapter/internal/fakedisk"
)

// fakeLLM поднимает OpenAI-совместимый сервер; записывает полученные messages.
func fakeLLM(t *testing.T, answer string, status int) (*chatllm.Client, *[][]chatllm.Msg) {
	t.Helper()
	var calls [][]chatllm.Msg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []chatllm.Msg `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body.Messages)
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": answer}}},
		})
	}))
	t.Cleanup(srv.Close)
	c := chatllm.New(chatllm.Config{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "gpt-oss-120b",
		System: "системный промпт", Retries: 2,
	})
	c.RetryPause = time.Millisecond
	return c, &calls
}

func seedChat(t *testing.T, f *fakedisk.Fake, id, text string, att bool, ctx []map[string]string) {
	t.Helper()
	env := map[string]any{
		"id": id, "ts": 1789743753, "kind": "chat", "text": text,
		"worker": "any", "context": ctx,
	}
	if att {
		env["attachments"] = []map[string]string{
			{"path": "in/att/" + id + "/photo.jpg", "name": "photo.jpg", "mime": "image/jpeg"},
		}
		f.Put(root+"/in/att/"+id+"/photo.jpg", []byte("jpg"))
	}
	body, _ := json.Marshal(env)
	f.Put(root+"/in/"+id+".json", body)
}

func outEnvelope(t *testing.T, f *fakedisk.Fake) map[string]any {
	t.Helper()
	for path, raw := range f.Files() {
		if strings.HasPrefix(path, root+"/out/") && strings.HasSuffix(path, ".json") {
			var env map[string]any
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("битый конверт %s: %v", path, err)
			}
			return env
		}
	}
	t.Fatal("конверт ответа не найден в out/")
	return nil
}

func TestChatEnvelopeRepliedViaLLM(t *testing.T) {
	f, p, st, _ := setup(t)
	llm, calls := fakeLLM(t, "ответ модели", http.StatusOK)
	p.Chat = llm

	seedChat(t, f, msgID, "как дела?", false, []map[string]string{
		{"role": "user", "content": "привет"},
	})
	n, err := p.PullIn(st)
	if err != nil || n != 1 {
		t.Fatalf("PullIn = %d, %v; want 1, nil", n, err)
	}
	env := outEnvelope(t, f)
	if env["ref"] != msgID || env["ok"] != true || env["worker"] != "llm" {
		t.Fatalf("env = %v", env)
	}
	if env["text"] != "ответ модели" {
		t.Fatalf("text = %v", env["text"])
	}
	if !f.HasFile(root + "/archive/in/" + msgID + ".json") {
		t.Fatal("исходный chat-конверт не заархивирован")
	}
	if !st.Has(msgID) {
		t.Fatal("id не в state")
	}
	if len(*calls) != 1 {
		t.Fatalf("LLM вызвана %d раз", len(*calls))
	}
	msgs := (*calls)[0]
	if len(msgs) != 3 || msgs[0].Role != "system" || msgs[2].Content != "как дела?" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestChatContextClampedToThree(t *testing.T) {
	f, p, st, _ := setup(t)
	llm, calls := fakeLLM(t, "ок", http.StatusOK)
	p.Chat = llm

	seedChat(t, f, msgID, "шесть", false, []map[string]string{
		{"role": "user", "content": "1"},
		{"role": "assistant", "content": "2"},
		{"role": "user", "content": "3"},
		{"role": "assistant", "content": "4"},
		{"role": "user", "content": "5"},
	})
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	msgs := (*calls)[0]
	// system + последние 3 (3,4,5) + текущий текст = 5
	if len(msgs) != 5 || msgs[1].Content != "3" || msgs[3].Content != "5" || msgs[4].Content != "шесть" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestChatAttachmentsRefused(t *testing.T) {
	f, p, st, _ := setup(t)
	llm, calls := fakeLLM(t, "не важно", http.StatusOK)
	p.Chat = llm

	seedChat(t, f, msgID, "с картинкой", true, nil)
	n, err := p.PullIn(st)
	if err != nil || n != 1 {
		t.Fatalf("PullIn = %d, %v; want 1, nil", n, err)
	}
	env := outEnvelope(t, f)
	if env["ok"] != false || !strings.Contains(env["text"].(string), "вложения") {
		t.Fatalf("env = %v", env)
	}
	if len(*calls) != 0 {
		t.Fatal("при вложениях LLM вызывать не нужно")
	}
}

func TestChatLLMDownSendsErrorReply(t *testing.T) {
	f, p, st, _ := setup(t)
	llm, _ := fakeLLM(t, "", http.StatusInternalServerError)
	p.Chat = llm

	seedChat(t, f, msgID, "привет", false, nil)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	env := outEnvelope(t, f)
	if env["ok"] != false || !strings.Contains(env["text"].(string), "LLM") {
		t.Fatalf("env = %v", env)
	}
	if !f.HasFile(root + "/archive/in/" + msgID + ".json") {
		t.Fatal("конверт не заархивирован — зациклится")
	}
}

func TestAgentKindGoesToQueueEvenWithChatConfigured(t *testing.T) {
	f, p, st, q := setup(t)
	llm, calls := fakeLLM(t, "не должно вызываться", http.StatusOK)
	p.Chat = llm

	seedTask(t, f, msgID, false)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(q, "inbox", msgID+".json")); err != nil {
		t.Fatalf("agent-задача не попала в очередь: %v", err)
	}
	for path := range f.Files() {
		if strings.HasPrefix(path, root+"/out/") {
			t.Fatalf("agent-задача отвечена LLM: %s", path)
		}
	}
	if len(*calls) != 0 {
		t.Fatal("LLM вызвана на agent-задаче")
	}
}
