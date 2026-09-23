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

	"yd-adapter/internal/chatllm"
	"yd-adapter/internal/notify"
	"yd-adapter/internal/stt"
	"yd-adapter/ydisk"
)

// Puller связывает клиент Диска, корень канала (/echo) и каталог очереди.
type Puller struct {
	Client   *ydisk.YdClient
	Root     string          // корень канала на Диске, напр. "/echo"
	QueueDir string          // локальный каталог очереди
	Chat     *chatllm.Client // nil — chat-конверты не поддерживаются
	STT      *stt.Client     // nil — голосовые конверты получают отказ
	Ntfy     *notify.Client  // nil — push-уведомления выключены
}

// конверт задачи/ответа на Диске (docs/PROTOCOL.md)
type Envelope struct {
	ID          string    `json:"id"`
	TS          int64     `json:"ts"`
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	Worker      string    `json:"worker"`
	Voice       bool      `json:"voice"` // голосовая задача: text пуст, аудио — во вложениях
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
	for _, d := range []string{"in", "in/att", "out", "archive/in", "archive/out", "archive/broken"} {
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
		if it.Type != "file" {
			continue
		}
		if !strings.HasSuffix(it.Name, ".json") {
			// не конверт: по протоколу (E6) — в archive/broken, не зацикливаемся
			log.Printf("WARN: %s/in/%s не конверт — перекладываю в archive/broken", p.Root, it.Name)
			if err := p.Client.Move(p.in(it.Name), p.archive("broken", it.Name)); err != nil && !isNameTaken(err) {
				return 0, err // до подсчёта обработанных ещё не дошли
			}
			continue
		}
		names = append(names, it.Name)
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
		case handlingQueued, handlingReplied:
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
	handlingReplied                  // chat-конверт отвечен через LLM
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

	if env.Kind == "chat" {
		return p.handleChat(name, id, env)
	}

	// голосовая задача: аудио расшифровывает адаптер, воркер получает текст
	if env.Voice {
		return p.handleVoice(name, id, env)
	}

	// вложения публикуются до конверта, но докачка может совпасть:
	// если чего-то нет — оставляем конверт в in/ на следующий цикл
	localAtts, deferred, err := p.downloadAtts(id, env.Attachments)
	if err != nil {
		return handlingDeferred, err
	}
	if deferred {
		return handlingDeferred, nil
	}

	if err := p.queueTask(id, env.Worker, env.Text, env.Context, localAtts); err != nil {
		return handlingDeferred, err
	}
	// задача уже в inbox: сбой перекладки в archive не должен приводить к повторной
	// постановке — state пометит id, следующий цикл дозархивирует (ветка st.Has)
	if err := p.Client.Move(p.in(name), p.archive("in", name)); err != nil && !isNameTaken(err) {
		log.Printf("WARN: задача %s в inbox, но конверт не переложен в archive: %v", id, err)
	}
	p.cleanupAtt(id)
	log.Printf("pull_in: задача %s (worker=%s, вложений %d) → inbox", id, env.Worker, len(localAtts))
	return handlingQueued, nil
}

// downloadAtts скачивает вложения конверта в files/<id>/in/.
// deferred=true — что-то не доехало, повтор на следующем цикле (без ошибки).
func (p *Puller) downloadAtts(id string, atts []Att) (local []Att, deferred bool, err error) {
	local = make([]Att, 0, len(atts))
	for _, att := range atts {
		src := p.Root + "/" + strings.TrimPrefix(att.Path, "/")
		dst := filepath.Join(p.QueueDir, "files", id, "in", att.Name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, false, err
		}
		out, err := os.Create(dst)
		if err != nil {
			return nil, false, err
		}
		if err := p.Client.Download(src, out); err != nil {
			out.Close()
			log.Printf("WARN: вложение %s не скачалось (%v) — задача %s отложена", att.Path, err, id)
			return nil, true, nil
		}
		if err := out.Close(); err != nil {
			return nil, false, err
		}
		local = append(local, Att{
			Path: filepath.ToSlash(filepath.Join("files", id, "in", att.Name)),
			Name: att.Name,
			Mime: att.Mime,
		})
	}
	return local, false, nil
}

// queueTask кладёт задачу в inbox атомарно (tmp + rename). Схема — как читает
// воркер (scripts/agent_poller.py): id, worker, task, context, attachments, chat_id.
func (p *Puller) queueTask(id, worker, text string, context []CtxItem, localAtts []Att) error {
	task := map[string]any{
		"id":          id,
		"worker":      worker,
		"task":        text,
		"context":     context,
		"attachments": localAtts,
		"chat_id":     0, // почтового чата больше нет; поле нужно схеме воркера
	}
	body, err := json.Marshal(task)
	if err != nil {
		return err
	}
	dst := filepath.Join(p.QueueDir, "inbox", id+".json")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// handleChat отвечает на конверт kind:chat через LLM, минуя очередь.
func (p *Puller) handleChat(name, id string, env Envelope) (handling, error) {
	// публикует отказ ok=false и архивирует исходник; сбой перекладки — не ошибка
	refuse := func(reason string) (handling, error) {
		envName, err := p.publishOutEnvelope(id, false, "llm", reason, nil)
		if err != nil {
			return handlingDeferred, err
		}
		if err := p.Client.Move(p.in(name), p.archive("in", name)); err != nil && !isNameTaken(err) {
			log.Printf("WARN: chat %s отвечен (%s), но конверт не переложен: %v", id, envName, err)
		}
		p.cleanupAtt(id)
		log.Printf("chat %s: отказ (%s), конверт %s", id, reason, envName)
		return handlingReplied, nil
	}

	if p.Chat == nil {
		return refuse("LLM на сервере не настроен")
	}
	if len(env.Attachments) > 0 {
		return refuse("вложения в чате пока не поддерживаются")
	}
	history := make([]chatllm.Msg, 0, len(env.Context))
	for _, c := range env.Context {
		history = append(history, chatllm.Msg{Role: c.Role, Content: c.Content})
	}
	answer, err := p.Chat.Answer(history, env.Text)
	if err != nil {
		return refuse("LLM недоступна: " + err.Error())
	}
	envName, err := p.publishOutEnvelope(id, true, "llm", answer, nil)
	if err != nil {
		return handlingDeferred, err
	}
	if err := p.Client.Move(p.in(name), p.archive("in", name)); err != nil && !isNameTaken(err) {
		log.Printf("WARN: chat %s отвечен (%s), но конверт не переложен: %v", id, envName, err)
	}
	p.cleanupAtt(id)
	log.Printf("chat %s: ответ LLM опубликован (%s)", id, envName)
	return handlingReplied, nil
}

// publishOutEnvelope публикует конверт ответа out/<ts>-<hash>.json (последним,
// правило 2). При 409 — новое имя. Возвращает имя опубликованного конверта.
func (p *Puller) publishOutEnvelope(refID string, ok bool, worker, text string, atts []Att) (string, error) {
	return p.publishOutEnvelopeEx(refID, ok, worker, text, atts, false, true)
}

// publishOutEnvelopeEx — расширенная публикация: voice помечает конверт как
// транскрипт голосового, notify выключает push (для промежуточных конвертов).
func (p *Puller) publishOutEnvelopeEx(refID string, ok bool, worker, text string, atts []Att, voice, notify bool) (string, error) {
	now := time.Now().Unix()
	for attempt := 0; ; attempt++ {
		envName := ydisk.MakeName(now, randomSalt())
		env := map[string]any{
			"id":          envName,
			"ref":         refID,
			"ts":          now,
			"ok":          ok,
			"worker":      worker,
			"text":        text,
			"attachments": atts,
		}
		if voice {
			env["voice"] = true
		}
		body, err := json.Marshal(env)
		if err != nil {
			return "", err
		}
		err = p.Client.Upload(p.Root+"/out/"+envName+".json", body)
		if err == nil {
			// push — best effort: конверт уже в out/, сбой уведомления не ошибка
			if notify && p.Ntfy != nil {
				if nerr := p.Ntfy.Reply(refID); nerr != nil {
					log.Printf("WARN: push не ушёл (%s): %v", refID, nerr)
				}
			}
			return envName, nil
		}
		if isNameTaken(err) && attempt < 5 {
			continue
		}
		return "", err
	}
}

func (p *Puller) download(name string) ([]byte, error) {
	var buf bytes.Buffer
	if err := p.Client.Download(p.in(name), &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (p *Puller) in(name string) string { return p.Root + "/in/" + name }

// cleanupAtt убирает каталог входных вложений обработанного конверта.
// Вложения уже скачаны в очередь; на Диске они больше не нужны (Delete на
// отсутствующий путь — no-op). Правило протокола: чистит только адаптер.
func (p *Puller) cleanupAtt(id string) {
	dir := p.Root + "/in/att/" + id
	if err := p.Client.Delete(dir); err != nil {
		log.Printf("WARN: не удалось убрать вложения %s: %v", dir, err)
	}
}

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
			// один сбойный результат не должен блокировать остальные: он остаётся
			// в outbox/ до следующего цикла, публикуем всё, что можем
			log.Printf("ERROR: результат %s не опубликован: %v", e.Name(), err)
			continue
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
		dst := p.Root + "/out/att/" + id + "/" + a.name
		if err := p.Client.EnsureDir(p.Root + "/out/att/" + id); err != nil {
			return false, err
		}
		// overwrite: путь вложения детерминирован (out/att/<id>/<имя>), и повторная
		// публикация после обрыва должна затирать прошлую попытку, а не падать 409
		// Потоком (UploadFile), а не []byte: канал держит до 2 ГБ на файл, и
		// os.ReadFile здесь съел бы всю память сервера.
		if err := p.Client.UploadFile(dst, a.path, true); err != nil {
			return false, fmt.Errorf("вложение %s: %w", dst, err)
		}
		diskAtts = append(diskAtts, Att{
			Path: "out/att/" + id + "/" + a.name,
			Name: a.name,
			Mime: a.mime,
		})
	}

	// 2. конверт ответа (последним)
	envName, err := p.publishOutEnvelope(id, res.OK, res.Worker, res.Text, diskAtts)
	if err != nil {
		return false, err
	}

	// 3. исходник → done/
	if err := os.Rename(src, filepath.Join(p.QueueDir, "done", name)); err != nil {
		return false, err
	}
	log.Printf("push_out: результат %s опубликован (%s), вложений %d", id, envName, len(diskAtts))
	return true, nil
}

// ---------- прогресс выполнения: маркеры в progress/ ----------

// ProgressState — маркер «агент взялся за задачу». Клиент по нему рисует
// «в работе» между «доставлено» и «есть ответ».
type ProgressState struct {
	ID     string `json:"id"`
	State  string `json:"state"` // "working"
	Worker string `json:"worker"`
	TS     int64  `json:"ts"`
}

// Progress публикует маркеры по локальным claim'ам и убирает маркеры задач,
// которые больше не в работе. Путь детерминирован (progress/<id>.json), поэтому
// запись — с overwrite=true; это best effort, источник истины — out/.
func (p *Puller) Progress() error {
	if err := p.Client.EnsureDir(p.Root + "/progress"); err != nil {
		return err
	}
	claimed, err := p.claimed()
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for id, worker := range claimed {
		body, err := json.Marshal(ProgressState{ID: id, State: "working", Worker: worker, TS: now})
		if err != nil {
			return err
		}
		if err := p.Client.UploadOverwrite(p.Root+"/progress/"+id+".json", body); err != nil {
			return fmt.Errorf("progress %s: %w", id, err)
		}
	}
	items, err := p.Client.List(p.Root + "/progress")
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Type != "file" || !strings.HasSuffix(it.Name, ".json") {
			continue
		}
		if _, ok := claimed[strings.TrimSuffix(it.Name, ".json")]; ok {
			continue
		}
		if err := p.Client.Delete(p.Root + "/progress/" + it.Name); err != nil {
			return err
		}
	}
	return nil
}

// claimed — активные захваты локальной очереди: id → worker.
func (p *Puller) claimed() (map[string]string, error) {
	entries, err := os.ReadDir(filepath.Join(p.QueueDir, "claimed"))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		worker, base, ok := splitClaim(e.Name())
		if !ok {
			continue
		}
		out[strings.TrimSuffix(base, ".json")] = worker
	}
	return out, nil
}

// splitClaim разбирает "<id>.json.<worker>"; ok=false для всего остального.
func splitClaim(name string) (worker, base string, ok bool) {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return "", "", false
	}
	base, worker = name[:i], name[i+1:]
	if worker == "" || !strings.HasSuffix(base, ".json") {
		return "", "", false
	}
	return worker, base, true
}

func randomSalt() string {
	b := make([]byte, 2)
	if _, err := crand.Read(b); err != nil {
		panic(err) // источник энтропии отсутствует — системе не место в строю
	}
	return hex.EncodeToString(b)
}
