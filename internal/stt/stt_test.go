package stt

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sttServer — тестовый сервер с заданным обработчиком /audio/transcriptions.
func sttServer(t *testing.T, handler http.HandlerFunc) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := New(Config{
		BaseURL: srv.URL,
		APIKey:  "sk-test",
		Model:   "whisper-1",
		Timeout: 5 * time.Second,
		Retries: 2,
	})
	c.RetryPause = time.Millisecond
	return c, srv.Close
}

// audioFile кладёт во временный каталог файл-пустышку и возвращает путь.
func audioFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("подготовка аудио: %v", err)
	}
	return p
}

func TestTranscribeSendsMultipartAndReturnsText(t *testing.T) {
	var gotAuth, gotModel, gotFilename string
	var gotFile []byte
	c, closeSrv := sttServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		gotModel = r.FormValue("model")
		f, hdr, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile: %v", err)
			http.Error(w, "no file", http.StatusBadRequest)
			return
		}
		defer f.Close()
		gotFilename = hdr.Filename
		gotFile, _ = io.ReadAll(f)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":"  привет мир  ","segments":[]}`)
	})
	defer closeSrv()

	path := audioFile(t, "voice.m4a", []byte("AUDIODATA"))
	out, err := c.Transcribe(path)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if out != "привет мир" {
		t.Fatalf("out = %q, want обрезанный текст", out)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotModel != "whisper-1" {
		t.Fatalf("model = %q", gotModel)
	}
	if gotFilename != "voice.m4a" {
		t.Fatalf("filename = %q", gotFilename)
	}
	if string(gotFile) != "AUDIODATA" {
		t.Fatalf("файл доехал как %q", gotFile)
	}
}

func TestTranscribeRetries429ThenSucceeds(t *testing.T) {
	var calls int
	c, closeSrv := sttServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"text":"ок"}`)
	})
	defer closeSrv()

	out, err := c.Transcribe(audioFile(t, "v.m4a", []byte("x")))
	if err != nil || out != "ок" {
		t.Fatalf("Transcribe = %q, %v; want ок, nil", out, err)
	}
	if calls != 3 {
		t.Fatalf("вызовов %d, want 3", calls)
	}
}

func TestTranscribeFailsAfterRetries(t *testing.T) {
	var calls int
	c, closeSrv := sttServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer closeSrv()

	_, err := c.Transcribe(audioFile(t, "v.m4a", []byte("x")))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want HTTP 500", err)
	}
	if calls != 3 {
		t.Fatalf("вызовов %d, want 3 (1 + 2 ретрая)", calls)
	}
}

func TestTranscribeDoesNotRetry4xx(t *testing.T) {
	var calls int
	c, closeSrv := sttServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "bad audio", http.StatusBadRequest)
	})
	defer closeSrv()

	_, err := c.Transcribe(audioFile(t, "v.m4a", []byte("x")))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want HTTP 400", err)
	}
	if calls != 1 {
		t.Fatalf("вызовов %d, want 1 (4xx не ретраится)", calls)
	}
}

func TestTranscribeEmptyTextIsError(t *testing.T) {
	c, closeSrv := sttServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"text":"   "}`)
	})
	defer closeSrv()

	_, err := c.Transcribe(audioFile(t, "v.m4a", []byte("x")))
	if err == nil || !strings.Contains(err.Error(), "пустой текст") {
		t.Fatalf("err = %v, want пустой текст", err)
	}
}

func TestTranscribeRejectsOversizeWithoutRequest(t *testing.T) {
	var calls int
	c, closeSrv := sttServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"text":"ок"}`)
	})
	defer closeSrv()
	c.cfg.MaxBytes = 4

	_, err := c.Transcribe(audioFile(t, "v.m4a", []byte("AUDIODATA")))
	if err == nil || !strings.Contains(err.Error(), "больше лимита") {
		t.Fatalf("err = %v, want больше лимита", err)
	}
	if calls != 0 {
		t.Fatalf("вызовов %d, want 0 — файл отклонён локально", calls)
	}
}
