package ydisk

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMakeName(t *testing.T) {
	got := MakeName(1789743753, "a1b2")
	if got != "20260918T150233Z-a1b2" {
		t.Fatalf("MakeName = %q, want 20260918T150233Z-a1b2", got)
	}
}

func TestParseNameRoundtrip(t *testing.T) {
	n := MakeName(1789743753, "a1b2")
	ts, salt, ok := ParseName(n)
	if !ok || ts != 1789743753 || salt != "a1b2" {
		t.Fatalf("ParseName(%q) = %d, %q, %v; want 1789743753, a1b2, true", n, ts, salt, ok)
	}
}

func TestParseNameRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"hello.txt", "20260918T150233Z", "20260918T150233Z-a1b2.json", "2026-09-18-a1b2"} {
		if _, _, ok := ParseName(bad); ok {
			t.Fatalf("ParseName(%q) не должен разбираться", bad)
		}
	}
}

func TestUploadNameTaken(t *testing.T) {
	var hrefGets int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hrefGets++
		http.Error(w, `{"description":"уже существует"}`, http.StatusConflict)
	}))
	defer api.Close()

	c := NewYdClient("tok", api.URL+"/v1/disk", api.Client())
	c.RetryPause = time.Millisecond
	err := c.Upload("/echo/x.txt", []byte("hi"))
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("want ErrNameTaken, got %v", err)
	}
	if hrefGets != 1 {
		t.Fatalf("409 не должен ретраиться, вызовов href: %d", hrefGets)
	}
}

func TestRetryOn429(t *testing.T) {
	var gets, puts int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/disk/resources/upload", func(w http.ResponseWriter, r *http.Request) {
		gets++
		if gets <= 2 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		fmt.Fprintf(w, `{"href":%q,"method":"PUT"}`, apiURL(r)+"/put")
	})
	mux.HandleFunc("/put", func(w http.ResponseWriter, r *http.Request) {
		puts++
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hi" {
			t.Errorf("PUT body = %q, want hi", body)
		}
		w.WriteHeader(http.StatusCreated)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	c := NewYdClient("tok", api.URL+"/v1/disk", api.Client())
	c.RetryPause = time.Millisecond
	if err := c.Upload("/echo/x.txt", []byte("hi")); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if gets != 3 || puts != 1 {
		t.Fatalf("gets=%d puts=%d, want 3 и 1", gets, puts)
	}
}

func TestEnsureDirCreatesParents(t *testing.T) {
	var created []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/disk/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		created = append(created, r.URL.Query().Get("path"))
		w.WriteHeader(http.StatusCreated)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	c := NewYdClient("tok", api.URL+"/v1/disk", api.Client())
	c.RetryPause = time.Millisecond
	if err := c.EnsureDir("/echo/in"); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if len(created) != 2 || created[0] != "/echo" || created[1] != "/echo/in" {
		t.Fatalf("создано %v, want [/echo /echo/in] — родители по уровням", created)
	}
}

func TestListPagination(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/disk/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "/echo/in" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("offset") {
		case "0":
			fmt.Fprintf(w, listing(2, 3, 0))
		case "2":
			fmt.Fprintf(w, listing(1, 3, 2))
		default:
			http.Error(w, "offset", http.StatusBadRequest)
		}
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	c := NewYdClient("tok", api.URL+"/v1/disk", api.Client())
	c.RetryPause = time.Millisecond
	items, err := c.List("/echo/in")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 3 || items[0].Name != "f0" || items[2].Name != "f2" {
		t.Fatalf("List вернул %+v, want 3 элемента f0..f2", items)
	}
}

func listing(n, total, offset int) string {
	s := `{"_embedded":{"total":` + itoa(total) + `,"offset":` + itoa(offset) + `,"items":[`
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += `{"name":"f` + itoa(offset+i) + `","type":"file","size":5}`
	}
	return s + `]}}`
}

func itoa(i int) string { return fmt.Sprint(i) }

func apiURL(r *http.Request) string { return "http://" + r.Host }
