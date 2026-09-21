package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yd-adapter/ydisk"
)

// результат в outbox: файл результата в files/<id>/out/ + json в outbox/
func seedResult(t *testing.T, q, id, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(q, "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(q, "files", id, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte("# отчёт"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id":      id,
		"chat_id": 0,
		"ok":      true,
		"worker":  "laptop",
		"text":    text,
		"attachments": []map[string]string{
			{"path": "files/" + id + "/out/report.md", "name": "report.md", "mime": ""},
		},
	})
	writeFile(t, filepath.Join(q, "outbox", id+".json"), string(body))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPushOutPublishesAttachmentsThenEnvelope(t *testing.T) {
	f, p, _, q := setup(t)
	id := msgID
	seedResult(t, q, id, "5 непрочитанных в 2 чатах задач")

	n, err := p.PushOut()
	if err != nil || n != 1 {
		t.Fatalf("PushOut = %d, %v; want 1, nil", n, err)
	}

	// вложение на Диске
	if !f.HasFile(root + "/out/att/" + id + "/report.md") {
		t.Fatal("вложение не опубликовано в out/att/")
	}

	// ровно один конверт в out/, имя разбирается, ref=id
	outDir := root + "/out"
	var envPath string
	for path := range f.Files() {
		if strings.HasPrefix(path, outDir+"/") && strings.HasSuffix(path, ".json") {
			if envPath != "" {
				t.Fatalf("лишний файл в out/: %s", path)
			}
			envPath = path
		}
	}
	if envPath == "" {
		t.Fatal("конверт ответа не опубликован")
	}
	envName := strings.TrimSuffix(filepath.Base(envPath), ".json")
	ts, salt, ok := ydisk.ParseName(envName)
	if !ok {
		t.Fatalf("имя конверта не по протоколу: %q", envName)
	}

	// содержимое конверта
	var env map[string]any
	raw := f.Files()[envPath]
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("битый конверт: %v", err)
	}
	if env["ref"] != id || env["ok"] != true || env["worker"] != "laptop" {
		t.Fatalf("env = %v", env)
	}
	if env["ts"] != float64(ts) {
		t.Fatalf("ts конверта %v != ts имени %d", env["ts"], ts)
	}
	if salt == "" {
		t.Fatal("пустой salt в имени")
	}

	// порядок: вложение раньше конверта (правило 2 протокола)
	jrnl := f.Journal()
	attIdx, envIdx := -1, -1
	for i, op := range jrnl {
		if strings.Contains(op, "/out/att/"+id+"/report.md") && attIdx < 0 {
			attIdx = i
		}
		if strings.Contains(op, "/out/"+envName+".json") && envIdx < 0 {
			envIdx = i
		}
	}
	if attIdx < 0 || envIdx < 0 || attIdx > envIdx {
		t.Fatalf("порядок нарушен: журнал %v (att=%d env=%d)", jrnl, attIdx, envIdx)
	}

	// исходник в done/, outbox пуст
	if _, err := os.Stat(filepath.Join(q, "done", id+".json")); err != nil {
		t.Fatal("outbox-файл не уехал в done/")
	}
	if _, err := os.Stat(filepath.Join(q, "outbox", id+".json")); !os.IsNotExist(err) {
		t.Fatal("outbox-файл остался на месте")
	}
}

func TestPushOutSecondRunIsNoop(t *testing.T) {
	f, p, _, q := setup(t)
	seedResult(t, q, msgID, "ответ")

	if _, err := p.PushOut(); err != nil {
		t.Fatalf("PushOut 1: %v", err)
	}
	n, err := p.PushOut()
	if err != nil || n != 0 {
		t.Fatalf("PushOut 2 = %d, %v; want 0", n, err)
	}
	if len(f.Files()) > 2 { // вложение + конверт, ничего нового
		t.Fatalf("лишние публикации: %v", f.Files())
	}
}

func TestPushOutMissingAttachmentKeepsResult(t *testing.T) {
	f, p, _, q := setup(t)
	// json есть, локального файла вложения нет
	if err := os.MkdirAll(filepath.Join(q, "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": msgID, "chat_id": 0, "ok": true, "worker": "laptop", "text": "ответ",
		"attachments": []map[string]string{
			{"path": "files/" + msgID + "/out/report.md", "name": "report.md", "mime": ""},
		},
	})
	writeFile(t, filepath.Join(q, "outbox", msgID+".json"), string(body))

	n, err := p.PushOut()
	if err != nil || n != 0 {
		t.Fatalf("PushOut = %d, %v; want 0 (отложено)", n, err)
	}
	if _, err := os.Stat(filepath.Join(q, "outbox", msgID+".json")); err != nil {
		t.Fatal("результат потерян из outbox")
	}
	if len(f.Files()) != 0 {
		t.Fatalf("что-то опубликовалось без вложения: %v", f.Files())
	}
}

// Повторная публикация: вложение уже на Диске (прошлая попытка) — overwrite
// затирает его, а не падает 409.
func TestPushOutRepublishOverwritesAttachment(t *testing.T) {
	f, p, _, q := setup(t)
	seedResult(t, q, msgID, "ответ")
	f.Put(root+"/out/att/"+msgID+"/report.md", []byte("прошлая попытка"))

	n, err := p.PushOut()
	if err != nil || n != 1 {
		t.Fatalf("PushOut = %d, %v; want 1, nil (повторная публикация)", n, err)
	}
	if got := string(f.Files()[root+"/out/att/"+msgID+"/report.md"]); got != "# отчёт" {
		t.Fatalf("вложение не перезаписано: %q", got)
	}
}

// Один сбойный результат не блокирует публикацию остальных и остаётся в outbox.
func TestPushOutContinuesAfterBadResult(t *testing.T) {
	f, p, _, q := setup(t)
	// плохой результат идёт первым по имени; вложение — каталог, ReadFile упадёт
	bad := "20260918T150000Z-baad"
	if err := os.MkdirAll(filepath.Join(q, "files", bad, "out", "report.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(q, "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(OutResult{
		ID: bad, OK: true, Worker: "laptop", Text: "плохой",
		Attachments: []Att{{Path: "files/" + bad + "/out/report.md", Name: "report.md"}},
	})
	writeFile(t, filepath.Join(q, "outbox", bad+".json"), string(body))
	seedResult(t, q, msgID, "хороший")

	n, err := p.PushOut()
	if err != nil {
		t.Fatalf("PushOut: %v", err)
	}
	if n != 1 {
		t.Fatalf("PushOut = %d, want 1 (хороший опубликован несмотря на сбойный)", n)
	}
	if _, err := os.Stat(filepath.Join(q, "outbox", bad+".json")); err != nil {
		t.Fatal("сбойный результат потерян из outbox")
	}
	if !f.HasFile(root + "/out/att/" + msgID + "/report.md") {
		t.Fatal("хороший результат не опубликован")
	}
}

func TestPushOutTextOnlyResult(t *testing.T) {
	f, p, _, q := setup(t)
	if err := os.MkdirAll(filepath.Join(q, "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": msgID, "chat_id": 0, "ok": false, "worker": "", "text": "нет воркера",
		"attachments": []map[string]string{},
	})
	writeFile(t, filepath.Join(q, "outbox", msgID+".json"), string(body))

	n, err := p.PushOut()
	if err != nil || n != 1 {
		t.Fatalf("PushOut = %d, %v; want 1", n, err)
	}
	var env map[string]any
	for path, raw := range f.Files() {
		if strings.HasPrefix(path, root+"/out/") {
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
		}
	}
	if env["ok"] != false {
		t.Fatalf("ok = %v, want false", env["ok"])
	}
}
