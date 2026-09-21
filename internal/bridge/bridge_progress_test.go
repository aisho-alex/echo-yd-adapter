package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// claim кладёт в очередь локальный claim как воркер.
func claim(t *testing.T, q, id, worker string) {
	t.Helper()
	dir := filepath.Join(q, "claimed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json."+worker), []byte("task"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProgressPublishesWorkingMarker(t *testing.T) {
	f, p, _, q := setup(t)
	claim(t, q, msgID, "laptop")

	if err := p.Progress(); err != nil {
		t.Fatalf("Progress: %v", err)
	}
	raw := f.Files()[root+"/progress/"+msgID+".json"]
	if raw == nil {
		t.Fatal("маркер прогресса не опубликован")
	}
	var st ProgressState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("битый маркер: %v", err)
	}
	if st.ID != msgID || st.State != "working" || st.Worker != "laptop" {
		t.Fatalf("маркер = %+v", st)
	}
}

func TestProgressRemovesMarkerWhenClaimGone(t *testing.T) {
	f, p, _, q := setup(t)
	claim(t, q, msgID, "laptop")
	if err := p.Progress(); err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if !f.HasFile(root + "/progress/" + msgID + ".json") {
		t.Fatal("маркер не появился")
	}

	// воркер снял claim (задача выполнена) — маркер должен уйти
	if err := os.Remove(filepath.Join(q, "claimed", msgID+".json.laptop")); err != nil {
		t.Fatal(err)
	}
	if err := p.Progress(); err != nil {
		t.Fatalf("Progress 2: %v", err)
	}
	if f.HasFile(root + "/progress/" + msgID + ".json") {
		t.Fatal("маркер остался после снятия claim")
	}
}

func TestProgressRemovesStaleMarker(t *testing.T) {
	f, p, _, _ := setup(t)
	// маркер от прошлого запуска адаптера: claim'а уже нет
	f.Put(root+"/progress/20260918T150401Z-c3d4.json", []byte(`{"id":"x"}`))

	if err := p.Progress(); err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if f.HasFile(root + "/progress/20260918T150401Z-c3d4.json") {
		t.Fatal("залежавшийся маркер не убран")
	}
}
