// Package bridge — мост «Яндекс.Диск ⇄ локальная очередь» /opt/echo-bot/queue.
// Схему задачи и результата очереди менять нельзя: её читает/пишет воркер
// (scripts/agent_poller.py). Протокол Диска — docs/PROTOCOL.md.
package bridge

import (
	"bytes"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

// ---------- публикация результатов: outbox → out ----------

// OutResult — схема результата в outbox/<id>.json (пишет воркер, менять нельзя).
type OutResult struct {
	ID          string `json:"id"`
	ChatID      int64  `json:"chat_id"`
	OK          bool   `json:"ok"`
	Worker      string `json:"worker"`
	Text        string `json:"text"`
	Attachments []Att  `json:"attachments"`
}

// PushOut публикует результаты из очереди на Диск: вложения (out/att/<id>/…)
// строго раньше конверта out/<ts>-<hash>.json (правило 2 протокола),
// затем outbox-файл уезжает в done/. Результат с недостающим вложением
// остаётся в outbox (не теряем). Возвращает число публикаций.
func (p *Puller) PushOut() (int, error) {
	outbox := filepath.Join(p.QueueDir, "outbox")
	entries, err := os.ReadDir(outbox)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// done/ нужен для перевода опубликованных результатов (адаптер сам себе дворник)
	if err := os.MkdirAll(filepath.Join(p.QueueDir, "done"), 0o755); err != nil {
		return 0, err
	}
	published := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ok, err := p.publishOne(outbox, e.Name())
		if err != nil {
			return published, err
		}
		if ok {
			published++
		}
	}
	return published, nil
}

func (p *Puller) publishOne(outbox, name string) (bool, error) {
	src := filepath.Join(outbox, name)
	raw, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	var res OutResult
	if err := json.Unmarshal(raw, &res); err != nil {
		log.Printf("WARN: битый результат outbox/%s — оставляю на месте: %v", name, err)
		return false, nil
	}
	id := strings.TrimSuffix(name, ".json")

	// все вложения должны быть на месте ДО публикации
	var local []struct{ path, name, mime string }
	for _, att := range res.Attachments {
		attPath := filepath.Join(p.QueueDir, filepath.FromSlash(att.Path))
		if _, err := os.Stat(attPath); err != nil {
			log.Printf("WARN: вложение %s результата %s не найдено — публикация отложена", att.Path, id)
			return false, nil
		}
		local = append(local, struct{ path, name, mime string }{attPath, att.Name, att.Mime})
	}

	// 1. вложения
	diskAtts := make([]Att, 0, len(local))
	for _, a := range local {
		data, err := os.ReadFile(a.path)
		if err != nil {
			return false, err
		}
		dst := p.Root + "/out/att/" + id + "/" + a.name
		if err := p.Client.EnsureDir(p.Root + "/out/att/" + id); err != nil {
			return false, err
		}
		if err := p.Client.Upload(dst, data); err != nil {
			return false, fmt.Errorf("вложение %s: %w", dst, err)
		}
		diskAtts = append(diskAtts, Att{
			Path: "out/att/" + id + "/" + a.name,
			Name: a.name,
			Mime: a.mime,
		})
	}

	// 2. конверт ответа (последним); имя уникально, при 409 — новое
	now := time.Now().Unix()
	var envName string
	for attempt := 0; ; attempt++ {
		envName = ydisk.MakeName(now, randomSalt())
		env := map[string]any{
			"id":          envName,
			"ref":         id,
			"ts":          now,
			"ok":          res.OK,
			"worker":      res.Worker,
			"text":        res.Text,
			"attachments": diskAtts,
		}
		body, err := json.Marshal(env)
		if err != nil {
			return false, err
		}
		err = p.Client.Upload(p.Root+"/out/"+envName+".json", body)
		if err == nil {
			break
		}
		if isNameTaken(err) && attempt < 5 {
			continue
		}
		return false, err
	}

	// 3. исходник → done/
	if err := os.Rename(src, filepath.Join(p.QueueDir, "done", name)); err != nil {
		return false, err
	}
	log.Printf("push_out: результат %s опубликован (%s), вложений %d", id, envName, len(diskAtts))
	return true, nil
}

func randomSalt() string {
	b := make([]byte, 2)
	if _, err := crand.Read(b); err != nil {
		panic(err) // источник энтропии отсутствует — системе не место в строю
	}
	return hex.EncodeToString(b)
}
