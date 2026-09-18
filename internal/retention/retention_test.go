package retention

import (
	"testing"
	"time"

	"yd-adapter/internal/fakedisk"
	"yd-adapter/ydisk"
)

const root = "/echo"

func setup(t *testing.T) (*fakedisk.Fake, *ydisk.YdClient) {
	t.Helper()
	f := fakedisk.New()
	t.Cleanup(f.Close)
	c := ydisk.NewYdClient("tok", f.URL(), nil)
	c.RetryPause = 0
	return f, c
}

func TestDeletesOnlyOldProtocolFiles(t *testing.T) {
	f, c := setup(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	oldName := ydisk.MakeName(now.Add(-40*24*time.Hour).Unix(), "aaaa")
	freshName := ydisk.MakeName(now.Add(-1*24*time.Hour).Unix(), "bbbb")
	inName := ydisk.MakeName(now.Add(-40*24*time.Hour).Unix(), "cccc")

	f.Put(root+"/archive/in/"+oldName+".json", []byte("{}"))
	f.Put(root+"/archive/out/"+freshName+".json", []byte("{}"))
	f.Put(root+"/archive/broken/"+inName+".json", []byte("{}"))
	f.Put(root+"/archive/in/readme.txt", []byte("не по протоколу"))
	// рабочие каталоги — не должны чиститься вообще
	f.Put(root+"/in/"+oldName+".json", []byte("{}"))
	f.Put(root+"/out/"+oldName+".json", []byte("{}"))

	n, err := Run(c, root, 30, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 2 {
		t.Fatalf("удалено %d, want 2 (archive/in и archive/broken)", n)
	}
	if f.HasFile(root + "/archive/in/" + oldName + ".json") {
		t.Fatal("старый archive/in не удалён")
	}
	if !f.HasFile(root + "/archive/out/" + freshName + ".json") {
		t.Fatal("свежий archive/out удалён")
	}
	if !f.HasFile(root + "/archive/in/readme.txt") {
		t.Fatal("файл не по протоколу тронут")
	}
	if !f.HasFile(root+"/in/"+oldName+".json") || !f.HasFile(root+"/out/"+oldName+".json") {
		t.Fatal("in/ или out/ очищены — ретеншн трогает только archive/")
	}
}

func TestBoundaryFreshStays(t *testing.T) {
	f, c := setup(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	// ровно 30 суток — не старше cutoff, остаётся
	name := ydisk.MakeName(now.Add(-30*24*time.Hour).Unix(), "dddd")
	f.Put(root+"/archive/in/"+name+".json", []byte("{}"))

	n, err := Run(c, root, 30, now)
	if err != nil || n != 0 {
		t.Fatalf("Run = %d, %v; want 0", n, err)
	}
	if !f.HasFile(root + "/archive/in/" + name + ".json") {
		t.Fatal("ровно на границе файл удалён")
	}
}
