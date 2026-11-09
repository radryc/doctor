package wal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

// MetricBatch is a group of metric point records staged together in the WAL.
type MetricBatch struct {
	ID         string                      `json:"id"`
	AppendedAt time.Time                   `json:"appended_at"`
	Records    []segment.MetricPointRecord `json:"records"`
}

// MetricBatchLog is a write-ahead log for buffered metric records.
// It uses length-prefixed JSON framing and group-commits fsyncs across
// concurrent appends.
type MetricBatchLog struct {
	log *commitLog[MetricBatch]
}

// NewMetricBatchLog creates a MetricBatchLog backed by files in dir.
func NewMetricBatchLog(dir string) (*MetricBatchLog, error) {
	if dir == "" {
		dir = ".doctor/metric-buffer"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("metric wal: create dir: %w", err)
	}
	return &MetricBatchLog{log: newCommitLog[MetricBatch](filepath.Join(dir, "metric-batches.log"), func(batch MetricBatch) string {
		return batch.ID
	})}, nil
}

// Append appends a batch to the WAL and returns its ID.
func (l *MetricBatchLog) Append(batch MetricBatch) (string, error) {
	if batch.ID == "" {
		batch.ID = newBatchID()
	}
	batch.AppendedAt = time.Now().UTC()
	data, err := json.Marshal(batch)
	if err != nil {
		return "", fmt.Errorf("metric wal: marshal batch: %w", err)
	}
	if err := l.log.append(batch.ID, data); err != nil {
		return "", err
	}
	return batch.ID, nil
}

// ReadAll returns all batches in the WAL.
func (l *MetricBatchLog) ReadAll() ([]MetricBatch, error) {
	return l.log.readAll()
}

// Iterate streams batches from the WAL in append order.
//
// The callback must not call back into the same MetricBatchLog.
func (l *MetricBatchLog) Iterate(yield func(MetricBatch) error) error {
	return l.log.iterate(yield)
}

// Retire marks already-flushed batches as consumed without rewriting the log.
func (l *MetricBatchLog) Retire(batches []MetricBatch) error {
	ids := make([]string, 0, len(batches))
	for _, batch := range batches {
		ids = append(ids, batch.ID)
	}
	return l.log.retire(ids)
}

// Replace atomically replaces the WAL contents with the given batches.
func (l *MetricBatchLog) Replace(batches []MetricBatch) error {
	ids := make([]string, 0, len(batches))
	payloads := make([][]byte, 0, len(batches))
	for _, batch := range batches {
		data, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("metric wal: marshal batch: %w", err)
		}
		ids = append(ids, batch.ID)
		payloads = append(payloads, data)
	}
	return l.log.replace(ids, payloads)
}

// Count returns the number of batches in the WAL.
func (l *MetricBatchLog) Count() int {
	n := 0
	_ = l.Iterate(func(MetricBatch) error { n++; return nil })
	return n
}
