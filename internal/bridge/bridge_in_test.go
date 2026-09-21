package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"yd-adapter/internal/fakedisk"
	"yd-adapter/ydisk"
)

const (
	root  = "/echo"
	msgID = "20260918T150233Z-a1b2"
)

// подготовка: фейковый Диск, клиент, временная очередь, чистое состояние
func setup(t *testing.T) (*fakedisk.Fake, *Puller, *State, string) {
	t.Helper()
	f := fakedisk.New()
	t.Cleanup(f.Close)
	c := ydisk.NewYdClient("tok", f.URL(), nil)
	c.RetryPause = 0
	q := t.TempDir()
	st := NewState("")
	return f, &Puller{Client: c, Root: root, QueueDir: q}, st, q
}

// конверт задачи с одним вложением кладём на фейковый Диск
func seedTask(t *testing.T, f *fakedisk.Fake, id string, att bool) {
	t.Helper()
	env := map[string]any{
		"id":     id,
		"ts":     1789743753,
		"kind":   "agent",
		"text":   "чекни новые сообщения в bitrix24",
		"worker": "any",
		"context": []map[string]string{
			{"role": "user", "content": "привет"},
		},
	}
	if att {
		env["attachments"] = []map[string]string{
			{"path": "in/att/" + id + "/photo.jpg", "name": "photo.jpg", "mime": "image/jpeg"},
		}
		f.Put(root+"/in/att/"+id+"/photo.jpg", []byte("\xff\xd8\xff\xe0JPGDATA"))
	}
	body, _ := json.Marshal(env)
	f.Put(root+"/in/"+id+".json", body)
}

func TestInEnvelopeBecomesQueueTask(t *testing.T) {
	f, p, st, q := setup(t)
	seedTask(t, f, msgID, true)

	n, err := p.PullIn(st)
	if err != nil || n != 1 {
		t.Fatalf("PullIn = %d, %v; want 1, nil", n, err)
	}

	raw, err := os.ReadFile(filepath.Join(q, "inbox", msgID+".json"))
	if err != nil {
		t.Fatalf("inbox задача не появилась: %v", err)
	}
	var task map[string]any
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("битая задача в inbox: %v", err)
	}
	if task["id"] != msgID || task["worker"] != "any" {
		t.Fatalf("task = %v", task)
	}
	if task["task"] != "чекни новые сообщения в bitrix24" {
		t.Fatalf("task.text = %v", task["task"])
	}
	if task["chat_id"] != float64(0) {
		t.Fatalf("chat_id = %v, want 0", task["chat_id"])
	}
	atts, _ := task["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments = %v", task["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["path"] != "files/"+msgID+"/in/photo.jpg" {
		t.Fatalf("attachment path = %v — воркер ждёт путь относительно очереди", att["path"])
	}
	data, err := os.ReadFile(filepath.Join(q, "files", msgID, "in", "photo.jpg"))
	if err != nil || string(data) != "\xff\xd8\xff\xe0JPGDATA" {
		t.Fatalf("вложение не доехало: %v %q", err, data)
	}

	// каталог входных вложений на Диске убран адаптером
	if f.HasFile(root + "/in/att/" + msgID + "/photo.jpg") {
		t.Fatal("in/att/<id>/ не очищен после обработки")
	}

	// tmp-файлов в inbox не остаётся
	entries, _ := os.ReadDir(filepath.Join(q, "inbox"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("остался tmp: %s", e.Name())
		}
	}
}

func TestProcessedEnvelopeMovesToArchiveAndState(t *testing.T) {
	f, p, st, _ := setup(t)
	seedTask(t, f, msgID, true)

	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	if !st.Has(msgID) {
		t.Fatal("id не записан в state")
	}
	if !f.HasFile(root + "/archive/in/" + msgID + ".json") {
		t.Fatal("исходный конверт не в archive/in")
	}
	if f.HasFile(root + "/in/" + msgID + ".json") {
		t.Fatal("исходный конверт остался в in/")
	}
}

func TestSecondPullIsNoop(t *testing.T) {
	f, p, st, q := setup(t)
	seedTask(t, f, msgID, true)

	if _, err := p.PullIn(st); err != nil {
		t.Fatalf("PullIn 1: %v", err)
	}
	n, err := p.PullIn(st)
	if err != nil || n != 0 {
		t.Fatalf("PullIn 2 = %d, %v; want 0, nil", n, err)
	}
	entries, _ := os.ReadDir(filepath.Join(q, "inbox"))
	if len(entries) != 1 {
		t.Fatalf("inbox вырос: %d файлов", len(entries))
	}
}

func TestBrokenJSONGoesToArchiveBroken(t *testing.T) {
	f, p, st, _ := setup(t)
	bad := "20260918T150300Z-ffff"
	f.Put(root+"/in/"+bad+".json", []byte("{не json"))
	// и не-json файл без .json вообще игнорируем
	f.Put(root+"/in/readme.txt", []byte("hi"))

	n, err := p.PullIn(st)
	if err != nil {
		t.Fatalf("адаптер не должен падать на битом JSON: %v", err)
	}
	if n != 0 {
		t.Fatalf("битый конверт посчитан обработанным: %d", n)
	}
	if !f.HasFile(root + "/archive/broken/" + bad + ".json") {
		t.Fatal("битый конверт не в archive/broken")
	}
	if !st.Has(bad) {
		t.Fatal("битый конверт не помечен в state — будет вечно перебираться")
	}
	// E6: файл без .json тоже уезжает в archive/broken, in/ не засоряется
	if !f.HasFile(root + "/archive/broken/readme.txt") {
		t.Fatal("не-json файл не убран в archive/broken")
	}
	if f.HasFile(root + "/in/readme.txt") {
		t.Fatal("не-json файл остался в in/")
	}
}
