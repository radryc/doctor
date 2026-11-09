package wal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

// LogBatch is a group of log records staged together in the WAL.
type LogBatch struct {
	ID         string              `json:"id"`
	AppendedAt time.Time           `json:"appended_at"`
	Records    []segment.LogRecord `json:"records"`
}

// LogBatchLog is a write-ahead log for buffered log records.
// It uses length-prefixed JSON framing and group-commits fsyncs across
// concurrent appends.
type LogBatchLog struct {
	log *commitLog[LogBatch]
}

// NewLogBatchLog creates a LogBatchLog backed by files in dir.
func NewLogBatchLog(dir string) (*LogBatchLog, error) {
	if dir == "" {
		dir = ".doctor/log-buffer"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("log wal: create dir: %w", err)
	}
	return &LogBatchLog{log: newCommitLog[LogBatch](filepath.Join(dir, "log-batches.log"), func(batch LogBatch) string {
		return batch.ID
	})}, nil
}

// Append appends a batch to the WAL and returns its ID.
func (l *LogBatchLog) Append(batch LogBatch) (string, error) {
	if batch.ID == "" {
		batch.ID = newBatchID()
	}
	batch.AppendedAt = time.Now().UTC()
	data, err := json.Marshal(batch)
	if err != nil {
		return "", fmt.Errorf("log wal: marshal batch: %w", err)
	}
	if err := l.log.append(batch.ID, data); err != nil {
		return "", err
	}
	return batch.ID, nil
}

// ReadAll returns all batches in the WAL.
func (l *LogBatchLog) ReadAll() ([]LogBatch, error) {
	return l.log.readAll()
}

// Iterate streams batches from the WAL in append order.
//
// The callback must not call back into the same LogBatchLog.
func (l *LogBatchLog) Iterate(yield func(LogBatch) error) error {
	return l.log.iterate(yield)
}

// Retire marks already-flushed batches as consumed. It advances a durable
// checkpoint instead of rewriting the log, and compacts the live tail only when
// enough dead bytes have accumulated.
func (l *LogBatchLog) Retire(batches []LogBatch) error {
	ids := make([]string, 0, len(batches))
	for _, batch := range batches {
		ids = append(ids, batch.ID)
	}
	return l.log.retire(ids)
}

// Replace atomically replaces the WAL contents with the given batches.
// Pass nil or an empty slice to truncate the log.
func (l *LogBatchLog) Replace(batches []LogBatch) error {
	ids := make([]string, 0, len(batches))
	payloads := make([][]byte, 0, len(batches))
	for _, batch := range batches {
		data, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("log wal: marshal batch: %w", err)
		}
		ids = append(ids, batch.ID)
		payloads = append(payloads, data)
	}
	return l.log.replace(ids, payloads)
}

// Count returns the number of batches in the WAL.
func (l *LogBatchLog) Count() int {
	n := 0
	_ = l.Iterate(func(LogBatch) error { n++; return nil })
	return n
}
