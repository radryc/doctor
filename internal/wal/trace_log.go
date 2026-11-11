package wal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

type TraceBatch struct {
	ID         string                `json:"id"`
	AppendedAt time.Time             `json:"appended_at"`
	Records    []segment.TraceRecord `json:"records"`
}

// TraceBatchLog is a write-ahead log for buffered trace records.
// It uses length-prefixed JSON framing and group-commits fsyncs across
// concurrent appends.
type TraceBatchLog struct {
	log *commitLog[TraceBatch]
}

func NewTraceBatchLog(dir string) (*TraceBatchLog, error) {
	if dir == "" {
		dir = ".doctor/trace-buffer"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("trace wal: create dir: %w", err)
	}
	return &TraceBatchLog{log: newCommitLog[TraceBatch](filepath.Join(dir, "trace-batches.log"), func(batch TraceBatch) string {
		return batch.ID
	})}, nil
}

func (l *TraceBatchLog) Append(batch TraceBatch) (string, error) {
	if batch.ID == "" {
		batch.ID = newBatchID()
	}
	batch.AppendedAt = time.Now().UTC()
	data, err := json.Marshal(batch)
	if err != nil {
		return "", fmt.Errorf("trace wal: marshal batch: %w", err)
	}
	if err := l.log.append(batch.ID, data); err != nil {
		return "", err
	}
	return batch.ID, nil
}

func (l *TraceBatchLog) ReadAll() ([]TraceBatch, error) {
	return l.log.readAll()
}

// Iterate streams batches from the WAL in append order.
//
// The callback must not call back into the same TraceBatchLog.
func (l *TraceBatchLog) Iterate(yield func(TraceBatch) error) error {
	return l.log.iterate(yield)
}

// Retire marks already-flushed batches as consumed without rewriting the log.
func (l *TraceBatchLog) Retire(batches []TraceBatch) error {
	ids := make([]string, 0, len(batches))
	for _, batch := range batches {
		ids = append(ids, batch.ID)
	}
	return l.log.retire(ids)
}

func (l *TraceBatchLog) Replace(batches []TraceBatch) error {
	ids := make([]string, 0, len(batches))
	payloads := make([][]byte, 0, len(batches))
	for _, batch := range batches {
		data, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("trace wal: marshal batch: %w", err)
		}
		ids = append(ids, batch.ID)
		payloads = append(payloads, data)
	}
	return l.log.replace(ids, payloads)
}

func (l *TraceBatchLog) Count() int {
	n := 0
	_ = l.Iterate(func(TraceBatch) error { n++; return nil })
	return n
}
