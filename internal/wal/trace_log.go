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

type TraceBatch struct {
	ID         string                `json:"id"`
	AppendedAt time.Time             `json:"appended_at"`
	Records    []segment.TraceRecord `json:"records"`
}

type TraceBatchLog struct {
	path string
	mu   sync.Mutex
}

func NewTraceBatchLog(dir string) (*TraceBatchLog, error) {
	if dir == "" {
		dir = ".doctor/trace-buffer"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("trace wal: create dir: %w", err)
	}
	return &TraceBatchLog{path: filepath.Join(dir, "trace-batches.log")}, nil
}

func (l *TraceBatchLog) Append(batch TraceBatch) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if batch.ID == "" {
		batch.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	batch.AppendedAt = time.Now().UTC()
	data, err := json.Marshal(batch)
	if err != nil {
		return "", fmt.Errorf("trace wal: marshal batch: %w", err)
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("trace wal: open append log: %w", err)
	}
	defer file.Close()
	if err := binary.Write(file, binary.LittleEndian, uint32(len(data))); err != nil {
		return "", fmt.Errorf("trace wal: write frame size: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return "", fmt.Errorf("trace wal: write frame: %w", err)
	}
	return batch.ID, file.Sync()
}

func (l *TraceBatchLog) ReadAll() ([]TraceBatch, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readAllLocked()
}

// Iterate streams batches from the WAL in append order.
//
// The callback must not call back into the same TraceBatchLog.
func (l *TraceBatchLog) Iterate(yield func(TraceBatch) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("trace wal: open log: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		var size uint32
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("trace wal: read frame size: %w", err)
		}

		payload := make([]byte, int(size))
		if _, err := io.ReadFull(reader, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("trace wal: read frame: %w", err)
		}

		var batch TraceBatch
		if err := json.Unmarshal(payload, &batch); err != nil {
			continue
		}
		if err := yield(batch); err != nil {
			return err
		}
	}
}

func (l *TraceBatchLog) Replace(batches []TraceBatch) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	tmpPath := l.path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("trace wal: open temp file: %w", err)
	}
	writer := bufio.NewWriter(file)
	for _, batch := range batches {
		data, err := json.Marshal(batch)
		if err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("trace wal: marshal batch: %w", err)
		}
		if err := binary.Write(writer, binary.LittleEndian, uint32(len(data))); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("trace wal: write frame size: %w", err)
		}
		if _, err := writer.Write(data); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("trace wal: write frame: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("trace wal: flush temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("trace wal: sync temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("trace wal: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("trace wal: replace log: %w", err)
	}
	return nil
}

func (l *TraceBatchLog) Count() int {
	n := 0
	_ = l.Iterate(func(TraceBatch) error { n++; return nil })
	return n
}

func (l *TraceBatchLog) readAllLocked() ([]TraceBatch, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("trace wal: open log: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var batches []TraceBatch
	for {
		var size uint32
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			if err == io.EOF {
				break
			}
			if err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("trace wal: read frame size: %w", err)
		}
		payload := make([]byte, int(size))
		if _, err := io.ReadFull(reader, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("trace wal: read frame: %w", err)
		}
		var batch TraceBatch
		if err := json.Unmarshal(payload, &batch); err != nil {
			return nil, fmt.Errorf("trace wal: decode frame: %w", err)
		}
		batches = append(batches, batch)
	}
	return batches, nil
}
