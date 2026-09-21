// Package reclaim — возвращает зависшие задачи из claimed/ в inbox/ и не даёт
// им зацикливаться. Роль, которую раньше выполнял бот: если воркер умер, не
// отчитавшись (heartbeat = touch по claim-файлу раз в 60 с), его claim протухает
// и задача должна выполниться заново.
//
// Защиты от прогонов по кругу:
//   - если результат задачи уже лежит в outbox/ или done/ — повторный запуск
//     агента не нужен: claim убирается, задача считается выполненной;
//   - задача, которую вернули в inbox ParkAfter раз, паркуется в parked/ —
//     аварийный стоп вместо бесконечного повтора.
package reclaim

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Reclaimer помнит, сколько раз каждая задача возвращалась в inbox.
// Счётчики живут в памяти: после рестарта адаптера цикл может повториться
// ещё ParkAfter раз — это осознанная плата за отсутствие лишнего состояния.
type Reclaimer struct {
	ParkAfter int // сколько возвратов терпеть; <=0 — не парковать
	counts    map[string]int
}

func New(parkAfter int) *Reclaimer {
	return &Reclaimer{ParkAfter: parkAfter, counts: map[string]int{}}
}

// Run перемещает просроченные claim'ы назад в inbox/. Claim — файл
// claimed/<id>.json.<worker>; возраст берётся по mtime (его обновляет
// heartbeat воркера). Возвращает число возвратов в inbox.
func (r *Reclaimer) Run(queueDir string, timeout time.Duration) (int, error) {
	claimed := filepath.Join(queueDir, "claimed")
	entries, err := os.ReadDir(claimed)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		worker, base, ok := parseClaim(e.Name())
		if !ok {
			continue // чужой файл в claimed/ — не трогаем
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := time.Since(info.ModTime())
		if age <= timeout {
			continue
		}
		id := strings.TrimSuffix(base, ".json")
		claimPath := filepath.Join(claimed, e.Name())

		// результат уже готов: перезапускать агента нечего, stale-claim убираем
		if r.resultReady(queueDir, id) {
			if err := os.Remove(claimPath); err != nil {
				return reclaimed, err
			}
			log.Printf("claim %s убран: результат уже готов (outbox/done)", id)
			continue
		}

		r.counts[id]++
		if r.ParkAfter > 0 && r.counts[id] >= r.ParkAfter {
			if err := r.park(queueDir, claimPath, base); err != nil {
				return reclaimed, err
			}
			log.Printf("WARN: задача %s возвращалась %d раз (worker %s) — запаркована в parked/",
				id, r.counts[id], worker)
			continue
		}

		if err := os.Rename(claimPath, filepath.Join(queueDir, "inbox", base)); err != nil {
			return reclaimed, err
		}
		log.Printf("task %s reclaimed from worker %s (no heartbeat for %ds)",
			id, worker, int(age.Seconds()))
		reclaimed++
	}
	if err := r.parkReadyInbox(queueDir); err != nil {
		return reclaimed, err
	}
	return reclaimed, nil
}

// parkReadyInbox убирает из inbox дубли уже выполненных задач (наследие старых
// реклеймов или падения между публикацией результата и снятием claim). Без этого
// воркер, запустившись, снова возьмёт их в работу и сожжёт прогон агента.
func (r *Reclaimer) parkReadyInbox(queueDir string) error {
	inbox := filepath.Join(queueDir, "inbox")
	entries, err := os.ReadDir(inbox)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !r.resultReady(queueDir, id) {
			continue
		}
		if err := r.park(queueDir, filepath.Join(inbox, e.Name()), e.Name()); err != nil {
			return err
		}
		log.Printf("WARN: дубль %s уже выполненной задачи убран из inbox в parked/", id)
	}
	return nil
}

// resultReady — есть ли уже результат задачи в outbox/ (ждёт публикации) или
// done/ (опубликован). Значит, прогон агента завершился и повтор не нужен.
func (r *Reclaimer) resultReady(queueDir, id string) bool {
	for _, dir := range []string{"outbox", "done"} {
		if _, err := os.Stat(filepath.Join(queueDir, dir, id+".json")); err == nil {
			return true
		}
	}
	return false
}

func (r *Reclaimer) park(queueDir, claimPath, base string) error {
	dir := filepath.Join(queueDir, "parked")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Rename(claimPath, filepath.Join(dir, base))
}

// parseClaim разбирает "<id>.json.<worker>"; ok=false для всего остального.
func parseClaim(name string) (worker, base string, ok bool) {
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
