package cull

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Session persists culling decisions so a run survives disconnects/restarts.
// Saved on every mutation (the file is tiny).
type Session struct {
	mu   sync.Mutex
	path string
	data sessionData
}

type sessionData struct {
	Decisions map[string]string `json:"decisions"` // shot ID -> "keep" | "reject"
	Cursor    int               `json:"cursor"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

func loadSession(path string) (*Session, error) {
	s := &Session{path: path, data: sessionData{Decisions: map[string]string{}}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", path, err)
	}
	if s.data.Decisions == nil {
		s.data.Decisions = map[string]string{}
	}
	return s, nil
}

func (s *Session) saveLocked() error {
	s.data.UpdatedAt = time.Now()
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SetDecision records a decision; decision "" clears it.
func (s *Session) SetDecision(id, decision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if decision == "" {
		delete(s.data.Decisions, id)
	} else {
		s.data.Decisions[id] = decision
	}
	return s.saveLocked()
}

func (s *Session) SetCursor(i int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Cursor == i {
		return nil
	}
	s.data.Cursor = i
	return s.saveLocked()
}

func (s *Session) Cursor() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Cursor
}

// Decisions returns a copy of the decisions map.
func (s *Session) Decisions() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.data.Decisions))
	for k, v := range s.data.Decisions {
		out[k] = v
	}
	return out
}
