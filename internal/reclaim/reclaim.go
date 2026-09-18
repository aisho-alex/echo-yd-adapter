// Package reclaim — возвращает зависшие задачи из claimed/ в inbox/.
// Роль, которую раньше выполнял бот: если воркер умер, не отчитавшись
// (heartbeat = touch по claim-файлу раз в 60 с), его claim протухает и
// задача должна выполниться заново.
package reclaim

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Run перемещает просроченные claim'ы назад в inbox/.
// Claim — файл claimed/<id>.json.<worker>; возраст берётся по mtime
// (его обновляет heartbeat воркера). Возвращает число возвратов.
func Run(queueDir string, timeout time.Duration) (int, error) {
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
		if err := os.Rename(
			filepath.Join(claimed, e.Name()),
			filepath.Join(queueDir, "inbox", base),
		); err != nil {
			return reclaimed, err
		}
		log.Printf("task %s reclaimed from worker %s (no heartbeat for %ds)",
			id, worker, int(age.Seconds()))
		reclaimed++
	}
	return reclaimed, nil
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
