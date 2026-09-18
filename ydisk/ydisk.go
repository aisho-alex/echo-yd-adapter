// Package ydisk — тонкий клиент Yandex Disk REST API v1 для файлового транспорта.
// Только стандартная библиотека. Правила протокола: см. docs/PROTOCOL.md.
package ydisk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrNameTaken — имя уже занято (Диск отвечает 409; встречается и 423).
var ErrNameTaken = errors.New("ydisk: имя занято")

// APIError — неуспешный ответ API с кодом и телом.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ydisk: HTTP %d: %s", e.Status, e.Body)
}

// Resource — элемент листинга каталога.
type Resource struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // "file" | "dir"
	Path     string `json:"path"`
	File     string `json:"file,omitempty"` // download URL для файлов
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
}

// YdClient — клиент REST API. Токен передаётся через конструктор, не из окружения
// (тестируемость). RetryPause — пауза перед первой ретрай-паузой (по умолчанию 1 c,
// в тестах уменьшается); дальше x2, x4. Ретраятся 429 и 5xx, всего 3 попытки.
type YdClient struct {
	token      string
	apiBase    string
	hc         *http.Client
	RetryPause time.Duration
}

func NewYdClient(token, apiBase string, hc *http.Client) *YdClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &YdClient{
		token:      token,
		apiBase:    strings.TrimSuffix(apiBase, "/"),
		hc:         hc,
		RetryPause: time.Second,
	}
}

// ---------- имена файлов протокола ----------

const nameLayout = "20060102T150405Z"

var nameRe = regexp.MustCompile(`^(\d{8}T\d{6}Z)-([0-9a-f]{4})$`)

// MakeName — "20260918T150233Z-a1b2": лексикографическая сортировка = хронология.
func MakeName(ts int64, salt string) string {
	return time.Unix(ts, 0).UTC().Format(nameLayout) + "-" + salt
}

// ParseName разбирает имя конверта; ok=false для всего, что не по формату.
func ParseName(name string) (ts int64, salt string, ok bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return 0, "", false
	}
	t, err := time.Parse(nameLayout, m[1])
	if err != nil {
		return 0, "", false
	}
	return t.Unix(), m[2], true
}

// ---------- HTTP-механика ----------

// do выполняет запрос с ретраями (3 попытки, паузы 1/2/4 c по умолчанию)
// на 429 и 5xx; сетевые ошибки считаются попыткой. 409/423 не ретраятся.
func (c *YdClient) do(method, rawURL string, body []byte) (*http.Response, error) {
	var resp *http.Response
	var err error
	pause := c.RetryPause
	const attempts = 3
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(pause)
			pause *= 2
		}
		var req *http.Request
		req, err = http.NewRequest(method, rawURL, bytesReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "OAuth "+c.token)
		if body != nil {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		resp, err = c.hc.Do(req)
		if err != nil {
			continue // сетевая ошибка — ретрай
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			drainClose(resp)
			continue
		}
		return resp, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ydisk: %s %s: %w", method, rawURL, err)
	}
	return nil, fmt.Errorf("ydisk: %s %s: исчерпаны попытки", method, rawURL)
}

// doAPI — запрос к apiBase с проверкой статуса.
func (c *YdClient) doAPI(method, pathQuery string, body []byte) (*http.Response, error) {
	resp, err := c.do(method, c.apiBase+pathQuery, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Body: readBody(resp.Body)}
	}
	return resp, nil
}

func isTaken(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusConflict || apiErr.Status == 423
	}
	return false
}

// ---------- операции протокола ----------

// Check — проверка токена: GET /v1/disk. 200 = токен живой и права на месте.
func (c *YdClient) Check() error {
	resp, err := c.doAPI(http.MethodGet, "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// List отдаёт содержимое каталога (с пагинацией).
func (c *YdClient) List(path string) ([]Resource, error) {
	var all []Resource
	offset := 0
	const limit = 100
	for page := 0; ; page++ {
		if page >= 200 { // защита от зацикливания
			return nil, fmt.Errorf("ydisk: List(%s): слишком много страниц", path)
		}
		resp, err := c.doAPI(http.MethodGet,
			"/resources?path="+url.QueryEscape(path)+
				"&limit="+strconv.Itoa(limit)+
				"&offset="+strconv.Itoa(offset), nil)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Embedded struct {
				Items []Resource `json:"items"`
				Total int        `json:"total"`
			} `json:"_embedded"`
		}
		err = json.NewDecoder(resp.Body).Decode(&parsed)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("ydisk: List(%s): %w", path, err)
		}
		all = append(all, parsed.Embedded.Items...)
		offset += len(parsed.Embedded.Items)
		if len(parsed.Embedded.Items) == 0 || offset >= parsed.Embedded.Total {
			return all, nil
		}
	}
}

// Upload грузит файл одним PUT (всегда overwrite=false). 409/423 → ErrNameTaken.
func (c *YdClient) Upload(path string, data []byte) error {
	resp, err := c.doAPI(http.MethodGet,
		"/resources/upload?path="+url.QueryEscape(path)+"&overwrite=false", nil)
	if err != nil {
		if isTaken(err) {
			return ErrNameTaken
		}
		return err
	}
	var href struct {
		Href string `json:"href"`
	}
	err = json.NewDecoder(resp.Body).Decode(&href)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("ydisk: Upload(%s): %w", path, err)
	}
	put, err := c.do(http.MethodPut, href.Href, data)
	if err != nil {
		return err
	}
	defer put.Body.Close()
	readBody(put.Body)
	if put.StatusCode >= 300 {
		if put.StatusCode == http.StatusConflict || put.StatusCode == 423 {
			return ErrNameTaken
		}
		return fmt.Errorf("ydisk: Upload(%s): PUT href: HTTP %d", path, put.StatusCode)
	}
	return nil
}

// Download скачивает файл (временная ссылка на downloader-хосте) в w.
func (c *YdClient) Download(path string, w io.Writer) error {
	resp, err := c.doAPI(http.MethodGet,
		"/resources/download?path="+url.QueryEscape(path), nil)
	if err != nil {
		return err
	}
	var href struct {
		Href string `json:"href"`
	}
	err = json.NewDecoder(resp.Body).Decode(&href)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("ydisk: Download(%s): %w", path, err)
	}
	get, err := c.do(http.MethodGet, href.Href, nil)
	if err != nil {
		return err
	}
	defer get.Body.Close()
	if get.StatusCode >= 300 {
		return fmt.Errorf("ydisk: Download(%s): GET href: HTTP %d", path, get.StatusCode)
	}
	_, err = io.Copy(w, get.Body)
	return err
}

// Move перемещает ресурс; overwrite=false. 409 → ErrNameTaken.
func (c *YdClient) Move(src, dst string) error {
	resp, err := c.doAPI(http.MethodPost,
		"/resources/move?from="+url.QueryEscape(src)+
			"&path="+url.QueryEscape(dst)+"&overwrite=false", nil)
	if err != nil {
		if isTaken(err) {
			return ErrNameTaken
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// Delete удаляет навсегда. Ответ 202 — удаление асинхронное (факт A3).
// 404 считается успехом (идемпотентность чистки).
func (c *YdClient) Delete(path string) error {
	resp, err := c.doAPI(http.MethodDelete,
		"/resources?path="+url.QueryEscape(path)+"&permanently=true", nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// EnsureDir создаёт каталог вместе с родителями по уровням: Диск не создаёт
// вложенные пути одним вызовом (факт A3). 409 на уровне = уже существует.
func (c *YdClient) EnsureDir(path string) error {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	cur := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		cur += "/" + p
		resp, err := c.doAPI(http.MethodPut,
			"/resources?path="+url.QueryEscape(cur), nil)
		if err != nil {
			if isTaken(err) {
				continue // уже существует
			}
			return fmt.Errorf("ydisk: EnsureDir(%s): %w", cur, err)
		}
		resp.Body.Close()
	}
	return nil
}

// ---------- утилиты ----------

func bytesReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return strings.NewReader(string(b))
}

func readBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	return strings.TrimSpace(string(b))
}

func drainClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
}
