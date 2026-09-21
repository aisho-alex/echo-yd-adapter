// Package notify — push-уведомления телефона о новых ответах через ntfy
// (публичный ntfy.sh; мгновенную доставку обеспечивает приложение-подписчик
// на телефоне). Текст ответа не отправляем — только факт и deep link:
// ntfy.sh видит лишь топик и заголовок. Push — best effort: ответ и так
// лежит в out/, телефон догонит опросом.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client отправляет уведомления в топик ntfy.
type Client struct {
	BaseURL    string        // напр. https://ntfy.sh
	Topic      string        // общий секрет: кто знает топик — может читать/слать
	RetryPause time.Duration // пауза перед повтором (по умолчанию 1 c)
	hc         *http.Client
}

func New(baseURL, topic string) *Client {
	if baseURL == "" {
		baseURL = "https://ntfy.sh"
	}
	return &Client{
		BaseURL:    baseURL,
		Topic:      topic,
		RetryPause: time.Second,
		hc:         &http.Client{Timeout: 5 * time.Second},
	}
}

// Reply уведомляет о новом ответе на задачу refID (click открывает приложение).
// Публикация как JSON — POST в корень ntfy (поля topic/click/message); POST в
// /<topic> ntfy считает тело сырым текстом и показывает его целиком.
// priority — число (4 = high): строка в JSON-публикации отвергается с HTTP 400.
func (c *Client) Reply(refID string) error {
	body, err := json.Marshal(map[string]any{
		"topic":    c.Topic,
		"title":    "echo-pult",
		"message":  "Новый ответ",
		"priority": 4,
		"tags":     []string{"envelope"},
		"click":    "echopult://open?ref=" + refID,
	})
	if err != nil {
		return err
	}
	var lastErr error
	pause := c.RetryPause
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(pause)
		}
		if lastErr = c.once(body); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (c *Client) once(body []byte) error {
	resp, err := c.hc.Post(strings.TrimSuffix(c.BaseURL, "/"),
		"application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}
