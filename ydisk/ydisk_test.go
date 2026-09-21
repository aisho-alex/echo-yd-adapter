package ydisk

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// UploadOverwrite шлёт overwrite=true (нужно для повторной публикации вложений),
// Upload остаётся строгим overwrite=false.
func TestUploadOverwriteSendsFlag(t *testing.T) {
	var gotOverwrite string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/disk/resources/upload", func(w http.ResponseWriter, r *http.Request) {
		gotOverwrite = r.URL.Query().Get("overwrite")
		fmt.Fprintf(w, `{"href":%q,"method":"PUT"}`, apiURL(r)+"/put")
	})
	mux.HandleFunc("/put", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	c := NewYdClient("tok", api.URL+"/v1/disk", api.Client())
	c.RetryPause = time.Millisecond

	if err := c.UploadOverwrite("/echo/out/att/x/report.md", []byte("hi")); err != nil {
		t.Fatalf("UploadOverwrite: %v", err)
	}
	if gotOverwrite != "true" {
		t.Fatalf("overwrite = %q, want true", gotOverwrite)
	}
	if err := c.Upload("/echo/x.txt", []byte("hi")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotOverwrite != "false" {
		t.Fatalf("overwrite = %q, want false (обычный Upload строгий)", gotOverwrite)
	}
}

// UploadFile грузит файл с диска ПОТОКОМ: содержимое доезжает байт в байт,
// размер известен заранее (Content-Length), а после 5xx на PUT файл
// переоткрывается — поток нельзя перемотать, поэтому ретрай обязан его взять заново.
func TestUploadFileStreamsAndRetries(t *testing.T) {
	payload := make([]byte, 5<<20) // 5 МБ: смысл в том, что файл не читается в память целиком
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	src := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	var puts int
	var gotLen int64
	var got []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/disk/resources/upload", func(w http.ResponseWriter, r *http.Request) {
		if ow := r.URL.Query().Get("overwrite"); ow != "true" {
			t.Errorf("overwrite = %q, want true", ow)
		}
		fmt.Fprintf(w, `{"href":%q,"method":"PUT"}`, apiURL(r)+"/put")
	})
	mux.HandleFunc("/put", func(w http.ResponseWriter, r *http.Request) {
		puts++
		if puts == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		gotLen = r.ContentLength
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	c := NewYdClient("tok", api.URL+"/v1/disk", api.Client())
	c.RetryPause = time.Millisecond
	if err := c.UploadFile("/echo/out/att/x/big.bin", src, true); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if puts != 2 {
		t.Fatalf("puts = %d, want 2 (после 5xx — повтор PUT)", puts)
	}
	if gotLen != int64(len(payload)) {
		t.Fatalf("Content-Length = %d, want %d", gotLen, len(payload))
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("тело PUT не совпало с файлом (получено %d байт)", len(got))
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
