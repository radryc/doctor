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

// LogBatch is a group of log records staged together in the WAL.
type LogBatch struct {
	ID         string              `json:"id"`
	AppendedAt time.Time           `json:"appended_at"`
	Records    []segment.LogRecord `json:"records"`
}

// LogBatchLog is a write-ahead log for buffered log records.
// It uses the same length-prefixed JSON framing as TraceBatchLog.
type LogBatchLog struct {
	path string
	mu   sync.Mutex
}

// NewLogBatchLog creates a LogBatchLog backed by files in dir.
func NewLogBatchLog(dir string) (*LogBatchLog, error) {
	if dir == "" {
		dir = ".doctor/log-buffer"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("log wal: create dir: %w", err)
	}
	return &LogBatchLog{path: filepath.Join(dir, "log-batches.log")}, nil
}

// Append appends a batch to the WAL and returns its ID.
func (l *LogBatchLog) Append(batch LogBatch) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if batch.ID == "" {
		batch.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	batch.AppendedAt = time.Now().UTC()
	data, err := json.Marshal(batch)
	if err != nil {
		return "", fmt.Errorf("log wal: marshal batch: %w", err)
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("log wal: open append log: %w", err)
	}
	defer file.Close()
	if err := binary.Write(file, binary.LittleEndian, uint32(len(data))); err != nil {
		return "", fmt.Errorf("log wal: write frame size: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return "", fmt.Errorf("log wal: write frame: %w", err)
	}
	return batch.ID, file.Sync()
}

// ReadAll returns all batches in the WAL.
func (l *LogBatchLog) ReadAll() ([]LogBatch, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readAllLocked()
}

// Iterate streams batches from the WAL in append order.
//
// The callback must not call back into the same LogBatchLog.
func (l *LogBatchLog) Iterate(yield func(LogBatch) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("log wal: open log: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		var size uint32
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("log wal: read frame size: %w", err)
		}

		payload := make([]byte, int(size))
		if _, err := io.ReadFull(reader, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("log wal: read frame: %w", err)
		}

		var batch LogBatch
		if err := json.Unmarshal(payload, &batch); err != nil {
			continue
		}
		if err := yield(batch); err != nil {
			return err
		}
	}
}

// Replace atomically replaces the WAL contents with the given batches.
// Pass nil or an empty slice to truncate the log.
func (l *LogBatchLog) Replace(batches []LogBatch) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	tmpPath := l.path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("log wal: open temp file: %w", err)
	}
	writer := bufio.NewWriter(file)
	for _, batch := range batches {
		data, err := json.Marshal(batch)
		if err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("log wal: marshal batch: %w", err)
		}
		if err := binary.Write(writer, binary.LittleEndian, uint32(len(data))); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("log wal: write frame size: %w", err)
		}
		if _, err := writer.Write(data); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("log wal: write frame: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("log wal: flush temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("log wal: sync temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("log wal: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("log wal: replace log: %w", err)
	}
	return nil
}

// Count returns the number of batches in the WAL.
func (l *LogBatchLog) Count() int {
	n := 0
	_ = l.Iterate(func(LogBatch) error { n++; return nil })
	return n
}

func (l *LogBatchLog) readAllLocked() ([]LogBatch, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("log wal: open log: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var batches []LogBatch
	for {
		var size uint32
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("log wal: read frame size: %w", err)
		}
		payload := make([]byte, int(size))
		if _, err := io.ReadFull(reader, payload); err != nil {
			break
		}
		var batch LogBatch
		if err := json.Unmarshal(payload, &batch); err != nil {
			continue
		}
		batches = append(batches, batch)
	}
	return batches, nil
}
