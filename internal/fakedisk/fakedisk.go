// Package fakedisk — фейковый Yandex Disk REST API на httptest для тестов
// адаптера. Повторяет поведение реального API, важное для протокола:
// 409 на повтор по занятому имени, 409 на mkdir без родителя,
// 202 на удаление каталога, href-схема upload/download.
package fakedisk

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Fake — подменный Disk. Доступ к хранилищу — только через методы
// (Put/Files/HasFile) для подготовки и проверки сценариев.
type Fake struct {
	svc   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
	jrnl  []string // журнал загрузок "put <path>" — проверка порядка в тестах
}

func New() *Fake {
	f := &Fake{
		files: map[string][]byte{},
		dirs:  map[string]bool{"/": true},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/disk/resources", f.handleResources)
	mux.HandleFunc("/v1/disk/resources/upload", f.handleUploadHref)
	mux.HandleFunc("/v1/disk/resources/download", f.handleDownloadHref)
	mux.HandleFunc("/v1/disk/resources/move", f.handleMove)
	mux.HandleFunc("/put", f.handlePut)
	mux.HandleFunc("/get", f.handleGet)
	f.svc = httptest.NewServer(mux)
	return f
}

// URL — база для NewYdClient (включая /v1/disk, как у реального API).
func (f *Fake) URL() string { return f.svc.URL + "/v1/disk" }

func (f *Fake) Close() { f.svc.Close() }

// Put кладёт файл напрямую в хранилище (подготовка сценариев).
func (f *Fake) Put(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = data
	f.addParentsLocked(path) // как при загрузке через API
}

// Files — снимок хранилища.
func (f *Fake) Files() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.files))
	for k, v := range f.files {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// HasFile — есть ли файл по пути.
func (f *Fake) HasFile(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[path]
	return ok
}

// ---------- обработчики ----------

func (f *Fake) handleResources(w http.ResponseWriter, r *http.Request) {
	path, err := url.QueryUnescape(r.URL.Query().Get("path"))
	if err != nil || path == "" || !strings.HasPrefix(path, "/") {
		httpError(w, http.StatusBadRequest, "bad path")
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.list(w, path)
	case http.MethodPut:
		f.mkdir(w, path)
	case http.MethodDelete:
		f.delete(w, path)
	default:
		httpError(w, http.StatusMethodNotAllowed, "method")
	}
}

func (f *Fake) list(w http.ResponseWriter, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path != "/" && !f.dirs[path] && !f.hasFileLocked(path) {
		httpError(w, http.StatusNotFound, "DiskNotFoundError")
		return
	}
	prefix := strings.TrimSuffix(path, "/") + "/"
	type item struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size,omitempty"`
	}
	var items []item
	for d := range f.dirs {
		if d != path && strings.HasPrefix(d, prefix) && !strings.Contains(strings.TrimPrefix(d, prefix), "/") {
			items = append(items, item{Name: strings.TrimPrefix(d, prefix), Type: "dir"})
		}
	}
	for p := range f.files {
		if strings.HasPrefix(p, prefix) && !strings.Contains(strings.TrimPrefix(p, prefix), "/") {
			items = append(items, item{Name: strings.TrimPrefix(p, prefix), Type: "file", Size: int64(len(f.files[p]))})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	if items == nil {
		items = []item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"_embedded": map[string]any{"items": items, "total": len(items), "offset": 0},
	})
}

func (f *Fake) mkdir(w http.ResponseWriter, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[path] || f.hasFileLocked(path) {
		httpError(w, http.StatusConflict, "уже существует")
		return
	}
	parent := parentOf(path)
	if parent != "/" && !f.dirs[parent] {
		// как у реального Диска: родителей сам не создаёт
		httpError(w, http.StatusConflict, "родитель не найден")
		return
	}
	f.dirs[path] = true
	w.WriteHeader(http.StatusCreated)
}

func (f *Fake) delete(w http.ResponseWriter, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	isDir := f.dirs[path]
	isFile := f.hasFileLocked(path)
	if !isDir && !isFile {
		httpError(w, http.StatusNotFound, "DiskNotFoundError")
		return
	}
	prefix := strings.TrimSuffix(path, "/") + "/"
	if isFile {
		delete(f.files, path)
	} else {
		delete(f.dirs, path)
		for d := range f.dirs {
			if strings.HasPrefix(d, prefix) {
				delete(f.dirs, d)
			}
		}
		for p := range f.files {
			if strings.HasPrefix(p, prefix) {
				delete(f.files, p)
			}
		}
	}
	// как у реального Диска: удаление каталога асинхронное
	w.WriteHeader(http.StatusAccepted)
}

func (f *Fake) handleUploadHref(w http.ResponseWriter, r *http.Request) {
	path, err := url.QueryUnescape(r.URL.Query().Get("path"))
	if err != nil || path == "" {
		httpError(w, http.StatusBadRequest, "bad path")
		return
	}
	overwrite := r.URL.Query().Get("overwrite")
	if overwrite != "false" && overwrite != "true" {
		httpError(w, http.StatusBadRequest, "overwrite должен быть true или false")
		return
	}
	f.mu.Lock()
	exists := f.dirs[path] || f.hasFileLocked(path)
	f.mu.Unlock()
	if exists && overwrite == "false" {
		httpError(w, http.StatusConflict, "уже существует")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"href":   f.svc.URL + "/put?path=" + url.QueryEscape(path),
		"method": "PUT",
	})
}

func (f *Fake) handlePut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		httpError(w, http.StatusMethodNotAllowed, "method")
		return
	}
	path, err := url.QueryUnescape(r.URL.Query().Get("path"))
	if err != nil || path == "" {
		httpError(w, http.StatusBadRequest, "bad path")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "body")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[path] {
		httpError(w, http.StatusConflict, "это каталог")
		return
	}
	f.files[path] = body
	f.jrnl = append(f.jrnl, "put "+path)
	// родители считаем существующими каталогами
	for cur := parentOf(path); cur != "/"; cur = parentOf(cur) {
		f.dirs[cur] = true
	}
	w.WriteHeader(http.StatusCreated)
}

// Journal — снимок журнала операций загрузки.
func (f *Fake) Journal() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.jrnl...)
}

func (f *Fake) handleDownloadHref(w http.ResponseWriter, r *http.Request) {
	path, err := url.QueryUnescape(r.URL.Query().Get("path"))
	if err != nil || path == "" {
		httpError(w, http.StatusBadRequest, "bad path")
		return
	}
	f.mu.Lock()
	_, ok := f.files[path]
	f.mu.Unlock()
	if !ok {
		httpError(w, http.StatusNotFound, "DiskNotFoundError")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"href": f.svc.URL + "/get?path=" + url.QueryEscape(path),
	})
}

func (f *Fake) handleGet(w http.ResponseWriter, r *http.Request) {
	path, err := url.QueryUnescape(r.URL.Query().Get("path"))
	if err != nil || path == "" {
		httpError(w, http.StatusBadRequest, "bad path")
		return
	}
	f.mu.Lock()
	data, ok := f.files[path]
	f.mu.Unlock()
	if !ok {
		httpError(w, http.StatusNotFound, "DiskNotFoundError")
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (f *Fake) handleMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "method")
		return
	}
	from, err1 := url.QueryUnescape(r.URL.Query().Get("from"))
	to, err2 := url.QueryUnescape(r.URL.Query().Get("path"))
	if err1 != nil || err2 != nil || from == "" || to == "" {
		httpError(w, http.StatusBadRequest, "bad args")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hasFileLocked(to) || f.dirs[to] {
		httpError(w, http.StatusConflict, "цель существует")
		return
	}
	if data, ok := f.files[from]; ok {
		delete(f.files, from)
		f.files[to] = data
		f.addParentsLocked(to)
		w.WriteHeader(http.StatusCreated)
		return
	}
	if !f.dirs[from] {
		httpError(w, http.StatusNotFound, "DiskNotFoundError")
		return
	}
	prefix := strings.TrimSuffix(from, "/") + "/"
	for d := range f.dirs {
		if strings.HasPrefix(d, prefix) {
			delete(f.dirs, d)
		}
	}
	delete(f.dirs, from)
	f.dirs[to] = true
	for p, data := range f.files {
		if strings.HasPrefix(p, prefix) {
			delete(f.files, p)
			f.files[to+strings.TrimPrefix(p, from)] = data
		}
	}
	f.addParentsLocked(to)
	w.WriteHeader(http.StatusCreated)
}

// ---------- утилиты ----------

func (f *Fake) hasFileLocked(path string) bool {
	_, ok := f.files[path]
	return ok
}

func (f *Fake) addParentsLocked(path string) {
	for cur := parentOf(path); cur != "/"; cur = parentOf(cur) {
		f.dirs[cur] = true
	}
}

func parentOf(path string) string {
	i := strings.LastIndex(strings.TrimSuffix(path, "/"), "/")
	if i <= 0 {
		return "/"
	}
	return path[:i]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error":       msg,
		"description": msg,
		"message":     msg,
	})
}
