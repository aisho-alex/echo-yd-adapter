// Package retention — чистка archive/ по сроку хранения. Возраст берётся из
// имени файла (ts в формате протокола), а не из mtime: Диск отдаёт mtime
// облака, который к протоколу отношения не имеет. Чистится только archive/.
package retention

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"yd-adapter/ydisk"
)

// Run удаляет из archive/{in,out,broken} файлы старше days дней.
// Файлы с именами не по протоколу не трогаются. Возвращает число удалённых.
func Run(c *ydisk.YdClient, root string, days int, now time.Time) (int, error) {
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	deleted := 0
	for _, dir := range []string{"archive/in", "archive/out", "archive/broken"} {
		items, err := c.List(root + "/" + dir)
		if err != nil {
			var apiErr *ydisk.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				continue // подкаталога архива ещё нет — чистить нечего
			}
			return deleted, err
		}
		for _, it := range items {
			if it.Type != "file" {
				continue
			}
			name := strings.TrimSuffix(it.Name, ".json")
			if name == it.Name {
				continue // не конверт — не наше дело
			}
			ts, _, ok := ydisk.ParseName(name)
			if !ok {
				continue // имя не по протоколу — не трогаем
			}
			if !time.Unix(ts, 0).Before(cutoff) {
				continue // ровно на границе и свежее — остаётся
			}
			if err := c.Delete(root + "/" + dir + "/" + it.Name); err != nil {
				return deleted, err
			}
			log.Printf("retention: удалён %s/%s (возраст больше %d дней)", dir, it.Name, days)
			deleted++
		}
	}
	return deleted, nil
}
