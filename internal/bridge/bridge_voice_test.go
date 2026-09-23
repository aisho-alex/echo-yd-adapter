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

	"yd-adapter/internal/fakedisk"
	"yd-adapter/internal/stt"
)

// fakeSTT поднимает OpenAI-совместимый /audio/transcriptions; считает вызовы
// и запоминает имя присланного файла.
func fakeSTT(t *testing.T, text string, status int) (*stt.Client, *int, *string) {
	t.Helper()
	calls := 0
	filename := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		calls++
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			if _, hdr, err := r.FormFile("file"); err == nil {
				filename = hdr.Filename
			}
		}
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"text": text})
	}))
	t.Cleanup(srv.Close)
	c := stt.New(stt.Config{BaseURL: srv.URL, APIKey: "sk-test", Model: "whisper-1", Retries: 2})
	c.RetryPause = time.Millisecond
	return c, &calls, &filename
}

// seedVoice кладёт голосовой конверт (kind:agent, voice:true) с указанным
// числом аудио-вложений.
func seedVoice(t *testing.T, f *fakedisk.Fake, id string, atts int) {
	t.Helper()
	env := map[string]any{
		"id": id, "ts": 1789743753, "kind": "agent", "text": "",
		"worker": "any", "voice": true,
	}
	var list []map[string]string
	for i := 0; i < atts; i++ {
		name := "voice.m4a"
		if i > 0 {
			name = "extra.m4a"
		}
		list = append(list, map[string]string{
			"path": "in/att/" + id + "/" + name, "name": name, "mime": "audio/mp4",
		})
		f.Put(root+"/in/att/"+id+"/"+name, []byte("AUDIODATA"))
	}
	if atts > 0 {
		env["attachments"] = list
	}
	body, _ := json.Marshal(env)
	f.Put(root+"/in/"+id+".json", body)
}

func TestVoiceTaskTranscribedToInbox(t *testing.T) {
	f, p, st, q := setup(t)
	c, calls, filename := fakeSTT(t, "найди письма в почте", http.StatusOK)
	p.STT = c

	seedVoice(t, f, msgID, 1)
	n, err := p.PullIn(st)
	if err != nil || n != 1 {
		t.Fatalf("PullIn = %d, %v; want 1, nil", n, err)
	}

	raw, err := os.ReadFile(filepath.Join(q, "inbox", msgID+".json"))
	if err != nil {
		t.Fatalf("голосовая задача не попала в inbox: %v", err)
	}
	var task map[string]any
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("битая задача: %v", err)
	}
	if task["task"] != "найди письма в почте" {
		t.Fatalf("task = %v, want транскрипт", task["task"])
	}
	// аудио остаётся вложением задачи и физически на месте
	atts, _ := task["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments = %v", task["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["path"] != "files/"+msgID+"/in/voice.m4a" {
		t.Fatalf("attachment path = %v", att["path"])
	}
	if _, err := os.Stat(filepath.Join(q, "files", msgID, "in", "voice.m4a")); err != nil {
		t.Fatalf("аудио не скачано в очередь: %v", err)
	}
	if *calls != 1 || *filename != "voice.m4a" {
		t.Fatalf("STT: вызовов %d, файл %q", *calls, *filename)
	}
	if !f.HasFile(root + "/archive/in/" + msgID + ".json") {
		t.Fatal("голосовой конверт не заархивирован")
	}
	if f.HasFile(root + "/in/att/" + msgID + "/voice.m4a") {
		t.Fatal("in/att/<id>/ не очищен")
	}
	if !st.Has(msgID) {
		t.Fatal("id не в state")
	}
}

func TestVoicePublishesTranscriptAck(t *testing.T) {
	f, p, st, _ := setup(t)
	c, _, _ := fakeSTT(t, "проверь почту", http.StatusOK)
	p.STT = c

	seedVoice(t, f, msgID, 1)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	var acks []map[string]any
	for path, raw := range f.Files() {
		if !strings.HasPrefix(path, root+"/out/") || !strings.HasSuffix(path, ".json") {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("битый конверт %s: %v", path, err)
		}
		if env["voice"] == true {
			acks = append(acks, env)
		}
	}
	if len(acks) != 1 {
		t.Fatalf("транскриптов-конвертов %d, want 1", len(acks))
	}
	ack := acks[0]
	if ack["ref"] != msgID || ack["ok"] != true || ack["worker"] != "stt" {
		t.Fatalf("ack = %v", ack)
	}
	if ack["text"] != "проверь почту" {
		t.Fatalf("ack.text = %v", ack["text"])
	}
}

func TestVoiceWithoutAudioGoesToBroken(t *testing.T) {
	f, p, st, q := setup(t)
	c, calls, _ := fakeSTT(t, "не важно", http.StatusOK)
	p.STT = c

	seedVoice(t, f, msgID, 0)
	n, err := p.PullIn(st)
	if err != nil {
		t.Fatalf("адаптер не должен падать на битом голосовом: %v", err)
	}
	if n != 0 {
		t.Fatalf("битый конверт посчитан обработанным: %d", n)
	}
	if !f.HasFile(root + "/archive/broken/" + msgID + ".json") {
		t.Fatal("голосовой конверт без аудио не в archive/broken")
	}
	if !st.Has(msgID) {
		t.Fatal("битый конверт не помечен в state — зациклится")
	}
	if *calls != 0 {
		t.Fatal("STT не должна вызываться на битом конверте")
	}
	if _, err := os.Stat(filepath.Join(q, "inbox", msgID+".json")); !os.IsNotExist(err) {
		t.Fatal("битый конверт попал в очередь")
	}
}

func TestVoiceWithTwoAudioGoesToBroken(t *testing.T) {
	f, p, st, _ := setup(t)
	c, _, _ := fakeSTT(t, "не важно", http.StatusOK)
	p.STT = c

	seedVoice(t, f, msgID, 2)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	if !f.HasFile(root + "/archive/broken/" + msgID + ".json") {
		t.Fatal("конверт с двумя аудио не уехал в archive/broken")
	}
}

func TestVoiceSttFailureSendsErrorReply(t *testing.T) {
	f, p, st, q := setup(t)
	c, _, _ := fakeSTT(t, "", http.StatusInternalServerError)
	p.STT = c

	seedVoice(t, f, msgID, 1)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	env := outEnvelope(t, f)
	if env["ok"] != false || !strings.Contains(env["text"].(string), "расшифровать") {
		t.Fatalf("env = %v", env)
	}
	if env["worker"] != "voice" {
		t.Fatalf("worker = %v, want voice", env["worker"])
	}
	if !f.HasFile(root + "/archive/in/" + msgID + ".json") {
		t.Fatal("конверт не заархивирован — зациклится")
	}
	if _, err := os.Stat(filepath.Join(q, "inbox", msgID+".json")); !os.IsNotExist(err) {
		t.Fatal("задача попала в очередь, хотя STT упала")
	}
	if _, err := os.Stat(filepath.Join(q, "files", msgID)); !os.IsNotExist(err) {
		t.Fatal("скачанные вложения отказа оставлены в очереди")
	}
}

func TestVoiceSttNotConfiguredRefuses(t *testing.T) {
	f, p, st, _ := setup(t)
	// p.STT == nil

	seedVoice(t, f, msgID, 1)
	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	env := outEnvelope(t, f)
	if env["ok"] != false || !strings.Contains(env["text"].(string), "не настроено") {
		t.Fatalf("env = %v", env)
	}
	if !f.HasFile(root + "/archive/in/" + msgID + ".json") {
		t.Fatal("конверт не заархивирован")
	}
}

func TestVoiceAudioDetectedByExtensionWhenMimeEmpty(t *testing.T) {
	f, p, st, q := setup(t)
	c, calls, _ := fakeSTT(t, "текст", http.StatusOK)
	p.STT = c

	env := map[string]any{
		"id": msgID, "ts": 1, "kind": "agent", "text": "", "worker": "any", "voice": true,
		"attachments": []map[string]string{
			{"path": "in/att/" + msgID + "/voice.ogg", "name": "voice.ogg", "mime": ""},
		},
	}
	f.Put(root+"/in/att/"+msgID+"/voice.ogg", []byte("OGG"))
	body, _ := json.Marshal(env)
	f.Put(root+"/in/"+msgID+".json", body)

	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("STT вызвана %d раз, want 1", *calls)
	}
	if _, err := os.ReadFile(filepath.Join(q, "inbox", msgID+".json")); err != nil {
		t.Fatalf("задача не в inbox: %v", err)
	}
}
