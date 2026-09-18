// Package chatllm — ответы на конверты kind:chat через OpenAI-совместимый API
// (тот же провайдер, что использовал бот: README LazDeltaChatBot «Ответы через LLM»).
// Серверную историю не ведём: системный промпт сервера + context из конверта + text.
package chatllm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Config — параметры подключения (из .env: LLM_*).
type Config struct {
	BaseURL   string // напр. https://api.neuraldeep.ru/v1
	APIKey    string
	Model     string
	System    string
	Timeout   time.Duration
	MaxTokens int
	Retries   int // дополнительные попытки на 429/5xx
}

// Msg — реплика для chat/completions.
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Client struct {
	cfg        Config
	hc         *http.Client
	RetryPause time.Duration // пауза перед первой ретрай-паузой (по умолчанию 1 c)
}

func New(cfg Config) *Client {
	if cfg.Model == "" {
		cfg.Model = "gpt-oss-120b"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 1024
	}
	return &Client{
		cfg:        cfg,
		hc:         &http.Client{Timeout: cfg.Timeout},
		RetryPause: time.Second,
	}
}

// Answer собирает запрос протокола: системный промпт сервера + последние
// ≤3 реплики контекста клиента + текст пользователя.
func (c *Client) Answer(history []Msg, text string) (string, error) {
	if len(history) > 3 {
		history = history[len(history)-3:]
	}
	msgs := make([]Msg, 0, 1+len(history)+1)
	if c.cfg.System != "" {
		msgs = append(msgs, Msg{Role: "system", Content: c.cfg.System})
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, Msg{Role: "user", Content: text})
	return c.Complete(msgs)
}

// Complete отправляет реплики в /chat/completions, возвращает content первого choice.
func (c *Client) Complete(msgs []Msg) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      c.cfg.Model,
		"messages":   msgs,
		"max_tokens": c.cfg.MaxTokens,
	})
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
		content, err := c.once(body)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !retryable(err) {
			return "", err
		}
	}
	return "", lastErr
}

func (c *Client) once(body []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err // сетевая ошибка/таймаут — ретраится
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &statusError{status: resp.StatusCode}
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("llm: разбор ответа: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm: пустой choices")
	}
	content := parsed.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("llm: пустой ответ модели")
	}
	return content, nil
}

type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("llm: HTTP %d", e.status) }

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusTooManyRequests || se.status >= 500
	}
	return true // сетевые ошибки ретраятся
}
