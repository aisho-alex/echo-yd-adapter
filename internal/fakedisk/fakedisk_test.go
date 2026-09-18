package fakedisk

import (
	"bytes"
	"errors"
	"testing"

	"yd-adapter/ydisk"
)

func newClient(t *testing.T) (*Fake, *ydisk.YdClient) {
	t.Helper()
	f := New()
	t.Cleanup(f.Close)
	c := ydisk.NewYdClient("tok", f.URL(), f.svc.Client())
	c.RetryPause = 0
	return f, c
}

// Тест-проверка заглушки (задача C2): список, загрузка, повтор → ErrNameTaken,
// скачивание (байты совпали), перемещение, удаление.
func TestFakeDiskCRUD(t *testing.T) {
	f, c := newClient(t)

	if err := c.EnsureDir("/echo/in"); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := c.EnsureDir("/echo/in"); err != nil {
		t.Fatalf("EnsureDir повторный: %v", err)
	}

	path := "/echo/in/20260918T150233Z-a1b2.json"
	if err := c.Upload(path, []byte(`{"id":"x"}`)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := c.Upload(path, []byte("dup")); !errors.Is(err, ydisk.ErrNameTaken) {
		t.Fatalf("повторная загрузка: want ErrNameTaken, got %v", err)
	}

	items, err := c.List("/echo/in")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Name != "20260918T150233Z-a1b2.json" || items[0].Size != 10 {
		t.Fatalf("List = %+v", items)
	}

	var buf bytes.Buffer
	if err := c.Download(path, &buf); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if buf.String() != `{"id":"x"}` {
		t.Fatalf("Download = %q", buf.String())
	}

	dst := "/echo/in/moved.json"
	if err := c.Move(path, dst); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if f.HasFile(path) {
		t.Fatal("исходник Move остался на месте")
	}
	if err := c.Move(dst, path); err != nil {
		t.Fatalf("Move обратно: %v", err)
	}

	if err := c.Delete("/echo/in"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if items, err := c.List("/echo/in"); err == nil && len(items) != 0 {
		t.Fatalf("после Delete List = %+v", items)
	}
	if f.HasFile(path) {
		t.Fatal("Delete не удалил файл")
	}
}

func TestFakeMkdirRequiresParent(t *testing.T) {
	_, c := newClient(t)
	// у реального Диска родители не создаются одним вызовом — фейк повторяет
	if err := c.EnsureDir("/echo"); err != nil {
		t.Fatalf("EnsureDir /echo: %v", err)
	}
	// а это должно сработать, т.к. EnsureDir идёт по уровням
	if err := c.EnsureDir("/echo/in/att"); err != nil {
		t.Fatalf("EnsureDir вложенный: %v", err)
	}
}
