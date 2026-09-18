// Package bridge — мост «Яндекс.Диск ⇄ локальная очередь» /opt/echo-bot/queue.
// Схему задачи и результата очереди менять нельзя: её читает/пишет воркер
// (scripts/agent_poller.py). Протокол Диска — docs/PROTOCOL.md.
package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"yd-adapter/ydisk"
)

// Puller связывает клиент Диска, корень канала (/echo) и каталог очереди.
type Puller struct {
	Client   *ydisk.YdClient
	Root     string // корень канала на Диске, напр. "/echo"
	QueueDir string // локальный каталог очереди
}

// конверт задачи/ответа на Диске (docs/PROTOCOL.md)
type Envelope struct {
	ID          string    `json:"id"`
	TS          int64     `json:"ts"`
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	Worker      string    `json:"worker"`
	Context     []CtxItem `json:"context"`
	Attachments []Att     `json:"attachments"`
}

type CtxItem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Att struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Mime string `json:"mime"`
}

// PullIn переносит новые задачи с Диска в локальную очередь.
// Возвращает число обработанных конвертов. Битые — в archive/broken,
// уже обработанные (state) — сразу в archive/in.
func (p *Puller) PullIn(st *State) (int, error) {
	for _, d := range []string{"inbox", "claimed", "outbox", "done", "files"} {
		if err := os.MkdirAll(filepath.Join(p.QueueDir, d), 0o755); err != nil {
			return 0, err
		}
	}
	for _, d := range []string{"in", "out", "archive/in", "archive/out", "archive/broken"} {
		if err := p.Client.EnsureDir(p.Root + "/" + d); err != nil {
			return 0, err
		}
	}

	items, err := p.Client.List(p.Root + "/in")
	if err != nil {
		return 0, err
	}
	var names []string
	for _, it := range items {
		if it.Type == "file" && strings.HasSuffix(it.Name, ".json") {
			names = append(names, it.Name)
		}
	}
	sort.Strings(names) // имя = хронология

	processed := 0
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		if st.Has(id) {
			// обработан ранее, но не заархивирован (падение между шагами)
			if err := p.Client.Move(p.in(name), p.archive("in", name)); err != nil && !isNameTaken(err) {
				return processed, err
			}
			continue
		}
		handled, err := p.processOne(name, id)
		if err != nil {
			return processed, err
		}
		switch handled {
		case handlingDeferred:
			continue // вложения не доехали, конверт остался в in/ — не помечаем
		case handlingQueued:
			processed++
		}
		if err := st.Mark(id); err != nil {
			return processed, err
		}
	}
	return processed, nil
}

// исход обработки одного конверта
type handling int

const (
	handlingDeferred handling = iota // вложения не скачались — повтор на следующем цикле
	handlingQueued                   // задача положена в очередь
	handlingBroken                   // битый JSON уехал в archive/broken
)

// processOne обрабатывает один конверт.
func (p *Puller) processOne(name, id string) (handling, error) {
	raw, err := p.download(name)
	if err != nil {
		return handlingDeferred, fmt.Errorf("скачивание %s: %w", name, err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Printf("WARN: битый JSON %s — в archive/broken: %v", name, err)
		return handlingBroken, p.Client.Move(p.in(name), p.archive("broken", name))
	}

	// вложения публикуются до конверта, но докачка может совпасть:
	// если чего-то нет — оставляем конверт в in/ на следующий цикл
	localAtts := make([]Att, 0, len(env.Attachments))
	for _, att := range env.Attachments {
		src := p.Root + "/" + strings.TrimPrefix(att.Path, "/")
		dst := filepath.Join(p.QueueDir, "files", id, "in", att.Name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return handlingDeferred, err
		}
		out, err := os.Create(dst)
		if err != nil {
			return handlingDeferred, err
		}
		if err := p.Client.Download(src, out); err != nil {
			out.Close()
			log.Printf("WARN: вложение %s не скачалось (%v) — задача %s отложена", att.Path, err, id)
			return handlingDeferred, nil
		}
		if err := out.Close(); err != nil {
			return handlingDeferred, err
		}
		localAtts = append(localAtts, Att{
			Path: filepath.ToSlash(filepath.Join("files", id, "in", att.Name)),
			Name: att.Name,
			Mime: att.Mime,
		})
	}

	task := map[string]any{
		"id":          id,
		"worker":      env.Worker,
		"task":        env.Text,
		"context":     env.Context,
		"attachments": localAtts,
		"chat_id":     0, // почтового чата больше нет; поле нужно схеме воркера
	}
	body, err := json.Marshal(task)
	if err != nil {
		return handlingDeferred, err
	}
	dst := filepath.Join(p.QueueDir, "inbox", id+".json")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return handlingDeferred, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return handlingDeferred, err
	}
	if err := p.Client.Move(p.in(name), p.archive("in", name)); err != nil && !isNameTaken(err) {
		return handlingQueued, err
	}
	log.Printf("pull_in: задача %s (worker=%s, вложений %d) → inbox", id, env.Worker, len(localAtts))
	return handlingQueued, nil
}

func (p *Puller) download(name string) ([]byte, error) {
	var buf bytes.Buffer
	if err := p.Client.Download(p.in(name), &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (p *Puller) in(name string) string { return p.Root + "/in/" + name }

func (p *Puller) archive(d, name string) string {
	return p.Root + "/archive/" + d + "/" + name
}

func isNameTaken(err error) bool { return err == ydisk.ErrNameTaken }
