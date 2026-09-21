package reclaim

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupQueue(t *testing.T) string {
	t.Helper()
	q := t.TempDir()
	for _, d := range []string{"inbox", "claimed", "outbox", "done", "files"} {
		if err := os.MkdirAll(filepath.Join(q, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return q
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func touchOld(t *testing.T, path string, age time.Duration) {
	t.Helper()
	past := time.Now().Add(-age)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}

// staleClaim кладёт просроченный claim и возвращает его путь.
func staleClaim(t *testing.T, q, id, worker string) string {
	t.Helper()
	claim := filepath.Join(q, "claimed", id+".json."+worker)
	writeFile(t, claim, "task")
	touchOld(t, claim, 2*time.Hour)
	return claim
}

func TestFreshClaimStays(t *testing.T) {
	q := setupQueue(t)
	claim := filepath.Join(q, "claimed", "20260918T150233Z-a1b2.json.laptop")
	writeFile(t, claim, "task")

	n, err := New(0).Run(q, 1800*time.Second)
	if err != nil || n != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", n, err)
	}
	if _, err := os.Stat(claim); err != nil {
		t.Fatal("свежий claim исчез")
	}
	if _, err := os.Stat(filepath.Join(q, "inbox", "20260918T150233Z-a1b2.json")); !os.IsNotExist(err) {
		t.Fatal("свежая задача уехала в inbox")
	}
}

func TestStaleClaimReturnsToInbox(t *testing.T) {
	q := setupQueue(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	claim := staleClaim(t, q, "20260918T150233Z-a1b2", "laptop")

	n, err := New(0).Run(q, 1800*time.Second)
	if err != nil || n != 1 {
		t.Fatalf("Run = %d, %v; want 1, nil", n, err)
	}
	if _, err := os.Stat(claim); !os.IsNotExist(err) {
		t.Fatal("просроченный claim остался в claimed/")
	}
	restored := filepath.Join(q, "inbox", "20260918T150233Z-a1b2.json")
	data, err := os.ReadFile(restored)
	if err != nil || string(data) != "task" {
		t.Fatalf("задача не вернулась в inbox: %v %q", err, data)
	}
	if !strings.Contains(logs.String(),
		"task 20260918T150233Z-a1b2 reclaimed from worker laptop (no heartbeat for ") {
		t.Fatalf("лог не по формату: %q", logs.String())
	}
}

// Результат уже готов (outbox/done): агента не перезапускаем, claim убираем.
func TestStaleClaimWithResultIsDropped(t *testing.T) {
	for _, dir := range []string{"outbox", "done"} {
		t.Run(dir, func(t *testing.T) {
			q := setupQueue(t)
			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			claim := staleClaim(t, q, "20260918T150233Z-a1b2", "laptop")
			writeFile(t, filepath.Join(q, dir, "20260918T150233Z-a1b2.json"), "{}")

			n, err := New(0).Run(q, 1800*time.Second)
			if err != nil || n != 0 {
				t.Fatalf("Run = %d, %v; want 0, nil (не перезапускать)", n, err)
			}
			if _, err := os.Stat(claim); !os.IsNotExist(err) {
				t.Fatal("claim с готовым результатом остался")
			}
			if _, err := os.Stat(filepath.Join(q, "inbox", "20260918T150233Z-a1b2.json")); !os.IsNotExist(err) {
				t.Fatal("задача вернулась в inbox, хотя результат готов")
			}
			if !strings.Contains(logs.String(), "результат уже готов") {
				t.Fatalf("нет пометки в логе: %q", logs.String())
			}
		})
	}
}

// Задача, которую реклеймили ParkAfter раз, паркуется — цикла больше нет.
func TestRepeatedReclaimParks(t *testing.T) {
	q := setupQueue(t)
	const id = "20260918T150233Z-a1b2"
	r := New(2)

	// первый возврат: счётчик 1 < 2 — задача едет в inbox
	staleClaim(t, q, id, "laptop")
	if n, err := r.Run(q, 1800*time.Second); err != nil || n != 1 {
		t.Fatalf("первый Run = %d, %v; want 1", n, err)
	}
	// воркер снова забрал задачу (mv убрал её из inbox) — эмулируем
	if err := os.Remove(filepath.Join(q, "inbox", id+".json")); err != nil {
		t.Fatal(err)
	}

	// воркер снова бросил: второй возврат попадает под парковку
	staleClaim(t, q, id, "laptop")
	n, err := r.Run(q, 1800*time.Second)
	if err != nil || n != 0 {
		t.Fatalf("второй Run = %d, %v; want 0 (запарковано)", n, err)
	}
	if _, err := os.Stat(filepath.Join(q, "parked", id+".json")); err != nil {
		t.Fatal("задача не запаркована")
	}
	if _, err := os.Stat(filepath.Join(q, "inbox", id+".json")); !os.IsNotExist(err) {
		t.Fatal("запаркованная задача вернулась в inbox")
	}
}

// Дубль уже выполненной задачи в inbox паркуется: воркер его не перезапустит.
func TestReadyDuplicateInInboxIsParked(t *testing.T) {
	q := setupQueue(t)
	const id = "20260918T150233Z-a1b2"
	writeFile(t, filepath.Join(q, "inbox", id+".json"), "task")
	writeFile(t, filepath.Join(q, "done", id+".json"), "{}")

	n, err := New(0).Run(q, 1800*time.Second)
	if err != nil || n != 0 {
		t.Fatalf("Run = %d, %v; want 0", n, err)
	}
	if _, err := os.Stat(filepath.Join(q, "inbox", id+".json")); !os.IsNotExist(err) {
		t.Fatal("дубль остался в inbox — воркер запустит агента заново")
	}
	if _, err := os.Stat(filepath.Join(q, "parked", id+".json")); err != nil {
		t.Fatal("дубль не запаркован")
	}
}

func TestOtherFilesAndDirsUntouched(t *testing.T) {
	q := setupQueue(t)

	// не-claim и странные файлы в claimed/
	writeFile(t, filepath.Join(q, "claimed", "notes.txt"), "hi")
	writeFile(t, filepath.Join(q, "claimed", "weird.json."), "trailing dot")
	writeFile(t, filepath.Join(q, "done", "20260918T150401Z-c3d4.json"), "done-file")
	writeFile(t, filepath.Join(q, "files", "keep.txt"), "keep")

	if _, err := New(0).Run(q, 1800*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, p := range []string{
		filepath.Join(q, "claimed", "notes.txt"),
		filepath.Join(q, "claimed", "weird.json."),
		filepath.Join(q, "done", "20260918T150401Z-c3d4.json"),
		filepath.Join(q, "files", "keep.txt"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("посторонний файл тронут: %s", p)
		}
	}
}

func TestMissingClaimedDirIsNoop(t *testing.T) {
	q := t.TempDir()
	n, err := New(0).Run(q, time.Second)
	if err != nil || n != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", n, err)
	}
}
