package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// journalEntry is one append-only accepted-bytes record. Seq is monotonic per
// replica and lets replay skip entries already captured by a snapshot.
type journalEntry struct {
	Seq       int64  `json:"seq"`
	Tenant    string `json:"tenant"`
	Partition string `json:"partition"`
	Day       string `json:"day"`
	Bytes     int64  `json:"bytes"`
}

type snapshotDoc struct {
	Seq     int64          `json:"seq"`
	Entries []journalEntry `json:"entries"`
}

// localState persists a replica's accepted bytes in a journal plus snapshot.
// It survives process restarts and crashes; entries are group-committed to disk
// by flush (run every FlushInterval and on shutdown).
type localState struct {
	mu           sync.Mutex
	dir          string
	replicaID    string
	snapshotPath string
	journalPath  string
	file         *os.File
	entries      map[stateKey]int64
	seq          int64
	snapSeq      int64
	flushCount   int64
}

func newLocalState(dir, replicaID string) (*localState, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("quota: state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("quota: create state directory %s: %w", dir, err)
	}
	replicaID = strings.TrimSpace(replicaID)
	if replicaID == "" {
		replicaID = "replica"
	}
	state := &localState{
		dir:          dir,
		replicaID:    replicaID,
		snapshotPath: filepath.Join(dir, "snapshot.json"),
		journalPath:  filepath.Join(dir, "journal-"+safeStateName(replicaID)+".log"),
		entries:      map[stateKey]int64{},
	}
	if err := state.replay(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(state.journalPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("quota: open journal %s: %w", state.journalPath, err)
	}
	state.file = file
	return state, nil
}

func (s *localState) replay() error {
	raw, err := os.ReadFile(s.snapshotPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("quota: read snapshot %s: %w", s.snapshotPath, err)
	}
	if err == nil {
		var doc snapshotDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("quota: parse snapshot %s: %w", s.snapshotPath, err)
		}
		for _, entry := range doc.Entries {
			s.entries[stateKey{Tenant: entry.Tenant, Partition: entry.Partition, Day: entry.Day}] += entry.Bytes
		}
		s.seq = doc.Seq
		s.snapSeq = doc.Seq
	}

	journal, err := os.ReadFile(s.journalPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("quota: read journal %s: %w", s.journalPath, err)
	}
	if err != nil {
		return nil
	}
	for index, line := range strings.Split(string(journal), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry journalEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return fmt.Errorf("quota: parse journal %s line %d: %w", s.journalPath, index+1, err)
		}
		if entry.Seq <= s.snapSeq {
			continue
		}
		s.entries[stateKey{Tenant: entry.Tenant, Partition: entry.Partition, Day: entry.Day}] += entry.Bytes
		if entry.Seq > s.seq {
			s.seq = entry.Seq
		}
	}
	return nil
}

func (s *localState) load(context.Context) error { return nil }

func (s *localState) snapshotMap() map[stateKey]int64 {
	out := make(map[stateKey]int64, len(s.entries))
	for key, value := range s.entries {
		out[key] = value
	}
	return out
}

func (s *localState) snapshot() map[stateKey]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotMap()
}

func (s *localState) add(_ context.Context, key stateKey, delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := journalEntry{
		Seq:       s.seq + 1,
		Tenant:    key.Tenant,
		Partition: key.Partition,
		Day:       key.Day,
		Bytes:     delta,
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("quota: encode journal entry: %w", err)
	}
	encoded = append(encoded, '\n')
	written, err := s.file.Write(encoded)
	if err != nil {
		return fmt.Errorf("quota: append journal %s: %w", s.journalPath, err)
	}
	if written != len(encoded) {
		return fmt.Errorf("quota: short journal write to %s", s.journalPath)
	}
	s.seq = entry.Seq
	s.entries[key] += delta
	return nil
}

func (s *localState) flush(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("quota: sync journal %s: %w", s.journalPath, err)
	}
	if err := s.writeSnapshotLocked(); err != nil {
		return err
	}
	// The snapshot carries Seq, so replay skips entries if truncation fails.
	if err := s.file.Truncate(0); err != nil {
		return fmt.Errorf("quota: truncate journal %s: %w", s.journalPath, err)
	}
	if _, err := s.file.Seek(0, 0); err != nil {
		return fmt.Errorf("quota: seek journal %s: %w", s.journalPath, err)
	}
	s.snapSeq = s.seq
	s.flushCount++
	return nil
}

func (s *localState) writeSnapshotLocked() error {
	doc := snapshotDoc{Seq: s.seq, Entries: make([]journalEntry, 0, len(s.entries))}
	for key, value := range s.entries {
		doc.Entries = append(doc.Entries, journalEntry{
			Tenant:    key.Tenant,
			Partition: key.Partition,
			Day:       key.Day,
			Bytes:     value,
		})
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("quota: encode snapshot: %w", err)
	}
	tmp := s.snapshotPath + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("quota: create snapshot %s: %w", tmp, err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return fmt.Errorf("quota: write snapshot %s: %w", tmp, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("quota: sync snapshot %s: %w", tmp, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("quota: close snapshot %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.snapshotPath); err != nil {
		return fmt.Errorf("quota: replace snapshot %s: %w", s.snapshotPath, err)
	}
	return nil
}

func (s *localState) close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	file := s.file
	s.file = nil
	return file.Close()
}

func (s *localState) describe() string {
	return "file:" + s.dir
}

func safeStateName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "replica"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}
