package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// State — множество уже обработанных id конвертов (идемпотентность рестартов).
// При заданном path хранится в JSON-файле; запись атомарная (tmp + rename).
type State struct {
	mu     sync.Mutex
	path   string
	seen   map[string]bool
	loaded bool
}

func NewState(path string) *State {
	return &State{path: path, seen: map[string]bool{}}
}

// Load дочитывает файл состояния (вызывается один раз на старте).
func (s *State) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		s.loaded = true
		return nil
	}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return err
	}
	for _, id := range ids {
		s.seen[id] = true
	}
	s.loaded = true
	return nil
}

// Count — сколько id уже обработано.
func (s *State) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func (s *State) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[id]
}

// Mark запоминает id; при заданном path — сохраняет файл атомарно.
func (s *State) Mark(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[id] = true
	if s.path == "" {
		return nil
	}
	ids := make([]string, 0, len(s.seen))
	for id := range s.seen {
		ids = append(ids, id)
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
