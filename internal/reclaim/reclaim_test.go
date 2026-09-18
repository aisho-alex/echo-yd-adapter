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
	for _, d := range []string{"inbox", "claimed", "done", "files"} {
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

func TestFreshClaimStays(t *testing.T) {
	q := setupQueue(t)
	claim := filepath.Join(q, "claimed", "20260918T150233Z-a1b2.json.laptop")
	writeFile(t, claim, "task")

	n, err := Run(q, 1800*time.Second)
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

	claim := filepath.Join(q, "claimed", "20260918T150233Z-a1b2.json.laptop")
	writeFile(t, claim, "task")
	touchOld(t, claim, 2*time.Hour)

	n, err := Run(q, 1800*time.Second)
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

func TestOtherFilesAndDirsUntouched(t *testing.T) {
	q := setupQueue(t)

	// не-claim и странные файлы в claimed/
	writeFile(t, filepath.Join(q, "claimed", "notes.txt"), "hi")
	writeFile(t, filepath.Join(q, "claimed", "weird.json."), "trailing dot")
	writeFile(t, filepath.Join(q, "done", "20260918T150401Z-c3d4.json"), "done-file")
	writeFile(t, filepath.Join(q, "files", "keep.txt"), "keep")

	if _, err := Run(q, 1800*time.Second); err != nil {
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
	n, err := Run(q, time.Second)
	if err != nil || n != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", n, err)
	}
}
