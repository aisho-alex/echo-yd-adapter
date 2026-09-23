// Package stt — распознавание речи для голосовых задач (kind:agent + voice).
// Провайдер — OpenAI-совместимый /audio/transcriptions (neuraldeep, whisper-1):
// multipart/form-data с полем file и полем model, ответ {text: ...}.
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config — параметры подключения (из .env: STT_*).
type Config struct {
	BaseURL  string // напр. https://api.neuraldeep.ru/v1
	APIKey   string
	Model    string
	Timeout  time.Duration
	MaxBytes int64 // лимит размера аудио; 0 — без лимита
	Retries  int   // дополнительные попытки на 429/5xx
}

type Client struct {
	cfg        Config
	hc         *http.Client
	RetryPause time.Duration // пауза перед первой ретрай-паузой (по умолчанию 1 c)
}

func New(cfg Config) *Client {
	if cfg.Model == "" {
		cfg.Model = "whisper-1"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 20 << 20 // 20 МБ: с запасом к лимиту провайдера (25 МБ)
	}
	return &Client{
		cfg:        cfg,
		hc:         &http.Client{Timeout: cfg.Timeout},
		RetryPause: time.Second,
	}
}

// Transcribe отправляет аудиофайл на распознавание и возвращает текст.
// Пустой текст и ошибка HTTP — ошибка: вызывающий отвечает ok=false.
func (c *Client) Transcribe(audioPath string) (string, error) {
	body, contentType, err := c.buildBody(audioPath)
	if err != nil {
		return "", err
	}

	var lastErr error
	pause := c.RetryPause
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			time.Sleep(pause)
			pause *= 2
		}
		text, err := c.once(body, contentType)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if !retryable(err) {
			return "", err
		}
	}
	return "", lastErr
}

// buildBody собирает multipart-тело один раз: размер ограничен MaxBytes,
// повторные попытки переиспользуют байты.
func (c *Client) buildBody(audioPath string) (body []byte, contentType string, err error) {
	info, err := os.Stat(audioPath)
	if err != nil {
		return nil, "", fmt.Errorf("stt: аудио недоступно: %w", err)
	}
	if c.cfg.MaxBytes > 0 && info.Size() > c.cfg.MaxBytes {
		return nil, "", fmt.Errorf("stt: аудио %d байт больше лимита %d", info.Size(), c.cfg.MaxBytes)
	}
	f, err := os.Open(audioPath)
	if err != nil {
		return nil, "", fmt.Errorf("stt: аудио недоступно: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, "", fmt.Errorf("stt: чтение аудио: %w", err)
	}
	if err := mw.WriteField("model", c.cfg.Model); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

func (c *Client) once(body []byte, contentType string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.cfg.BaseURL, "/")+"/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err // сетевая ошибка/таймаут — ретраится
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", &statusError{status: resp.StatusCode, body: strings.TrimSpace(string(raw))}
	}
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("stt: разбор ответа: %w", err)
	}
	text := strings.TrimSpace(parsed.Text)
	if text == "" {
		return "", fmt.Errorf("stt: пустой текст распознавания")
	}
	return text, nil
}

type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("stt: HTTP %d", e.status)
	}
	return fmt.Sprintf("stt: HTTP %d: %s", e.status, e.body)
}

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusTooManyRequests || se.status >= 500
	}
	return true // сетевые ошибки ретраятся
}
