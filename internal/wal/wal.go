package wal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry represents a pending write operation staged in the WAL.
type Entry struct {
	ID        string    `json:"id"`
	Signal    string    `json:"signal"`
	Tenant    string    `json:"tenant"`
	ObjectKey string    `json:"object_key"`
	Content   []byte    `json:"content"`
	Manifest  []byte    `json:"manifest"`
	StagedAt  time.Time `json:"staged_at"`
}

// WAL provides write-ahead logging for segment writes. Operations are first
// staged as pending entries on local disk and only removed after successful
// commit to the object store and catalog.
type WAL struct {
	dir string
	mu  sync.Mutex
}

// New creates a WAL backed by the given directory.
func New(dir string) (*WAL, error) {
	if dir == "" {
		dir = ".doctor/wal"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: create dir: %w", err)
	}
	return &WAL{dir: dir}, nil
}

// Stage writes a pending entry to the WAL. Returns the entry ID.
func (w *WAL) Stage(entry Entry) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	entry.StagedAt = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("wal: marshal entry: %w", err)
	}
	path := filepath.Join(w.dir, entry.ID+".pending")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("wal: write pending: %w", err)
	}
	return entry.ID, nil
}

// Commit marks a WAL entry as successfully written by removing the pending file.
func (w *WAL) Commit(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	path := filepath.Join(w.dir, id+".pending")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("wal: commit %s: %w", id, err)
	}
	return nil
}

// Pending returns all uncommitted entries, sorted by staged time.
func (w *WAL) Pending() ([]Entry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("wal: list dir: %w", err)
	}
	var pending []Entry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pending") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(w.dir, e.Name()))
		if err != nil {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(data, &entry); err != nil {
			continue
		}
		pending = append(pending, entry)
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].StagedAt.Before(pending[j].StagedAt)
	})
	return pending, nil
}

// Count returns the number of pending entries.
func (w *WAL) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pending") {
			n++
		}
	}
	return n
}
