package wal

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// MetricBatch is a group of metric point records staged together in the WAL.
type MetricBatch struct {
	ID         string                      `json:"id"`
	AppendedAt time.Time                   `json:"appended_at"`
	Records    []segment.MetricPointRecord `json:"records"`
}

// MetricBatchLog is a write-ahead log for buffered metric records.
// It uses the same length-prefixed JSON framing as TraceBatchLog.
type MetricBatchLog struct {
	path string
	mu   sync.Mutex
}

// NewMetricBatchLog creates a MetricBatchLog backed by files in dir.
func NewMetricBatchLog(dir string) (*MetricBatchLog, error) {
	if dir == "" {
		dir = ".doctor/metric-buffer"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("metric wal: create dir: %w", err)
	}
	return &MetricBatchLog{path: filepath.Join(dir, "metric-batches.log")}, nil
}

// Append appends a batch to the WAL and returns its ID.
func (l *MetricBatchLog) Append(batch MetricBatch) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if batch.ID == "" {
		batch.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	batch.AppendedAt = time.Now().UTC()
	data, err := json.Marshal(batch)
	if err != nil {
		return "", fmt.Errorf("metric wal: marshal batch: %w", err)
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("metric wal: open append log: %w", err)
	}
	defer file.Close()
	if err := binary.Write(file, binary.LittleEndian, uint32(len(data))); err != nil {
		return "", fmt.Errorf("metric wal: write frame size: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return "", fmt.Errorf("metric wal: write frame: %w", err)
	}
	return batch.ID, file.Sync()
}

// ReadAll returns all batches in the WAL.
func (l *MetricBatchLog) ReadAll() ([]MetricBatch, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readAllLocked()
}

// Iterate streams batches from the WAL in append order.
//
// The callback must not call back into the same MetricBatchLog.
func (l *MetricBatchLog) Iterate(yield func(MetricBatch) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("metric wal: open log: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		var size uint32
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("metric wal: read frame size: %w", err)
		}

		payload := make([]byte, int(size))
		if _, err := io.ReadFull(reader, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("metric wal: read frame: %w", err)
		}

		var batch MetricBatch
		if err := json.Unmarshal(payload, &batch); err != nil {
			continue
		}
		if err := yield(batch); err != nil {
			return err
		}
	}
}

// Replace atomically replaces the WAL contents with the given batches.
func (l *MetricBatchLog) Replace(batches []MetricBatch) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	tmpPath := l.path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("metric wal: open temp file: %w", err)
	}
	writer := bufio.NewWriter(file)
	for _, batch := range batches {
		data, err := json.Marshal(batch)
		if err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("metric wal: marshal batch: %w", err)
		}
		if err := binary.Write(writer, binary.LittleEndian, uint32(len(data))); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("metric wal: write frame size: %w", err)
		}
		if _, err := writer.Write(data); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("metric wal: write frame: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("metric wal: flush temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("metric wal: sync temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("metric wal: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("metric wal: replace log: %w", err)
	}
	return nil
}

// Count returns the number of batches in the WAL.
func (l *MetricBatchLog) Count() int {
	n := 0
	_ = l.Iterate(func(MetricBatch) error { n++; return nil })
	return n
}

func (l *MetricBatchLog) readAllLocked() ([]MetricBatch, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("metric wal: open log: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var batches []MetricBatch
	for {
		var size uint32
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("metric wal: read frame size: %w", err)
		}
		payload := make([]byte, int(size))
		if _, err := io.ReadFull(reader, payload); err != nil {
			break
		}
		var batch MetricBatch
		if err := json.Unmarshal(payload, &batch); err != nil {
			continue
		}
		batches = append(batches, batch)
	}
	return batches, nil
}
