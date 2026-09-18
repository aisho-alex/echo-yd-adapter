package bridge

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Полный локальный прогон (C8, шаг 3): фейковый Диск + поддельная очередь.
// Сценарий: задача с вложением, chat-задача, битый JSON, затем результат
// "воркера" с файлом. Ничего не должно потеряться.
func TestFullCycleNothingLost(t *testing.T) {
	f, p, st, q := setup(t)
	llm, _ := fakeLLM(t, "привет из LLM", http.StatusOK)
	p.Chat = llm

	chatID := "20260918T150400Z-bbbb"
	badID := "20260918T150500Z-cccc"
	seedTask(t, f, msgID, true)                       // agent + вложение
	seedChat(t, f, chatID, "болтаем", false, nil)     // chat
	f.Put(root+"/in/"+badID+".json", []byte("{oops")) // битый

	pulled, err := p.PullIn(st)
	if err != nil {
		t.Fatalf("PullIn: %v", err)
	}
	if pulled != 2 {
		t.Fatalf("pull_in = %d, want 2 (agent + chat; битый не в счёт)", pulled)
	}

	// 1. agent-задача в inbox, вложение у воркера
	if _, err := os.Stat(filepath.Join(q, "inbox", msgID+".json")); err != nil {
		t.Fatal("agent-задача не в inbox")
	}
	if _, err := os.Stat(filepath.Join(q, "files", msgID, "in", "photo.jpg")); err != nil {
		t.Fatal("вложение задачи не в files/<id>/in/")
	}
	// 2. chat отвечен прямо на Диске
	env := outEnvelope(t, f)
	if env["ref"] != chatID || env["text"] != "привет из LLM" {
		t.Fatalf("chat-ответ = %v", env)
	}
	// 3. битый — в archive/broken, помечен, адаптер жив
	if !f.HasFile(root + "/archive/broken/" + badID + ".json") {
		t.Fatal("битый конверт не в archive/broken")
	}
	if !st.Has(msgID) || !st.Has(chatID) || !st.Has(badID) {
		t.Fatalf("state неполный: %d записей", st.Count())
	}

	// воркер отработал: файл результата и запись в outbox
	seedResult(t, q, msgID, "отчёт готов")

	// воркер забрал задачу (claim) и отработал: файл результата и запись в outbox
	if err := os.Rename(
		filepath.Join(q, "inbox", msgID+".json"),
		filepath.Join(q, "claimed", msgID+".json.laptop"),
	); err != nil {
		t.Fatalf("worker claim: %v", err)
	}
	seedResult(t, q, msgID, "отчёт готов")

	pushed, err := p.PushOut()
	if err != nil || pushed != 1 {
		t.Fatalf("PushOut = %d, %v; want 1", pushed, err)
	}
	// воркер снял claim после публикации (drop_claim в agent_poller)
	if err := os.Remove(filepath.Join(q, "claimed", msgID+".json.laptop")); err != nil {
		t.Fatalf("drop_claim: %v", err)
	}
	if !f.HasFile(root + "/out/att/" + msgID + "/report.md") {
		t.Fatal("вложение результата не опубликовано")
	}
	if _, err := os.Stat(filepath.Join(q, "done", msgID+".json")); err != nil {
		t.Fatal("обработанный результат не в done/")
	}

	// ничего не осталось в незавершённом состоянии
	if f.HasFile(root+"/in/"+msgID+".json") || f.HasFile(root+"/in/"+chatID+".json") {
		t.Fatal("входные конверты не заархивированы")
	}
	for _, d := range []string{"inbox", "claimed", "outbox"} {
		entries, _ := os.ReadDir(filepath.Join(q, d))
		if len(entries) != 0 {
			t.Fatalf("%s не пуст: %v", d, entries)
		}
	}
	// в out/ два конверта (chat + результат агента), оба по протоколу
	outCount := 0
	for path := range f.Files() {
		if strings.HasPrefix(path, root+"/out/") && strings.HasSuffix(path, ".json") {
			outCount++
		}
	}
	if outCount != 2 {
		t.Fatalf("в out/ %d конвертов, want 2", outCount)
	}
}
