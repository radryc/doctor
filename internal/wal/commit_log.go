package wal

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// batchIDCounter makes IDs unique even when two appends read the same clock
// tick. A time-based prefix keeps them roughly sortable in logs.
var batchIDCounter atomic.Uint64

func newBatchID() string {
	return fmt.Sprintf("%d-%d", time.Now().UTC().UnixNano(), batchIDCounter.Add(1))
}

// maxWALRetainedBytes bounds the dead prefix left behind by checkpointing.
// Once more than this many flushed bytes remain in the file, Replace compacts
// the live tail into a fresh file.
const maxWALRetainedBytes = 64 << 20

type batchOffset struct {
	start int64
	end   int64
}

// commitLog is the shared length-prefixed JSON WAL implementation used by the
// log, trace, and metric batch logs.
//
// Appends are group-committed: every appender writes its frame under the mutex
// and registers a completion channel, but only one goroutine performs the
// fsync. All appends written before that fsync complete together.
//
// Flushed batches are retired with a durable checkpoint offset rather than
// rewriting the whole WAL. Replace() advances the checkpoint when the batches
// it is given are a contiguous run that this log appended; otherwise it falls
// back to a full rewrite (for example, after a restart or an arbitrary edit).
type commitLog[T any] struct {
	path        string
	checkpoint  string
	idOf        func(T) string
	maxRetained int64

	mu           sync.Mutex
	cond         *sync.Cond
	file         *os.File
	size         int64
	checkpointAt int64
	syncing      bool
	pending      []chan error
	offsets      map[string]batchOffset
}

func newCommitLog[T any](path string, idOf func(T) string) *commitLog[T] {
	log := &commitLog[T]{
		path:        path,
		checkpoint:  path + ".checkpoint",
		idOf:        idOf,
		maxRetained: maxWALRetainedBytes,
		offsets:     make(map[string]batchOffset),
	}
	if offset, err := readWALCheckpoint(log.checkpoint); err == nil {
		log.checkpointAt = offset
	}
	return log
}

func readWALCheckpoint(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	offset, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("wal: invalid checkpoint %q", string(data))
	}
	return offset, nil
}

func writeWALCheckpoint(path string, offset int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(strconv.FormatInt(offset, 10)); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (l *commitLog[T]) append(id string, payload []byte) error {
	l.mu.Lock()
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
	if l.file == nil {
		file, size, err := openWALAppendFile(l.path)
		if err != nil {
			l.mu.Unlock()
			return fmt.Errorf("wal: open append log: %w", err)
		}
		if l.checkpointAt > size {
			l.checkpointAt = 0
			_ = writeWALCheckpoint(l.checkpoint, 0)
		}
		l.file = file
		l.size = size
	}
	start := l.size
	if err := l.writeFrameLocked(payload); err != nil {
		l.mu.Unlock()
		return err
	}
	if id != "" {
		l.offsets[id] = batchOffset{start: start, end: l.size}
	}

	done := make(chan error, 1)
	l.pending = append(l.pending, done)
	startSync := !l.syncing
	if startSync {
		l.syncing = true
	}
	l.mu.Unlock()

	if startSync {
		l.runSync()
	}
	return <-done
}

func openWALAppendFile(path string) (*os.File, int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func (l *commitLog[T]) writeFrameLocked(payload []byte) error {
	var frame [4]byte
	binary.LittleEndian.PutUint32(frame[:], uint32(len(payload)))
	if _, err := l.file.Write(frame[:]); err != nil {
		l.abortWriteLocked()
		return fmt.Errorf("wal: write frame size: %w", err)
	}
	if _, err := l.file.Write(payload); err != nil {
		l.abortWriteLocked()
		return fmt.Errorf("wal: write frame: %w", err)
	}
	l.size += int64(len(frame) + len(payload))
	return nil
}

// abortWriteLocked discards a partial frame so the next append cannot land
// behind corrupt data and replay cannot stop early.
func (l *commitLog[T]) abortWriteLocked() {
	if l.file == nil {
		return
	}
	_ = l.file.Truncate(l.size)
	_ = l.file.Close()
	l.file = nil
}

// runSync drains the current append generation and then picks up any appends
// that arrived while fsync was in progress.
func (l *commitLog[T]) runSync() {
	for {
		l.mu.Lock()
		if len(l.pending) == 0 {
			l.syncing = false
			l.cond.Broadcast()
			l.mu.Unlock()
			return
		}
		waiters := l.pending
		l.pending = nil
		file := l.file
		l.mu.Unlock()

		var err error
		if file == nil {
			err = errors.New("wal: append log is not open")
		} else {
			err = file.Sync()
		}
		for _, done := range waiters {
			done <- err
		}
	}
}

// replace atomically replaces the log contents with the given batches. This is
// the strict, general-purpose path used by tests and explicit rewrites.
func (l *commitLog[T]) replace(ids []string, payloads [][]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
	for l.syncing {
		l.cond.Wait()
	}
	return l.rewriteLocked(ids, payloads)
}

// retire marks the given batches as flushed without rewriting the WAL. It moves
// the durable checkpoint to the earliest frame that must be kept, including any
// append that reached the WAL but has not yet been added to the caller's
// in-memory buffer. When enough dead bytes accumulate it compacts by copying
// the live tail instead of re-encoding every batch.
func (l *commitLog[T]) retire(ids []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
	for l.syncing {
		l.cond.Wait()
	}

	start, ok := l.checkpointForLocked(ids)
	if !ok {
		// Unknown IDs (for example before Replay has populated offsets). Keep
		// the WAL intact; the next flush can try again.
		return nil
	}
	if start < l.checkpointAt {
		start = l.checkpointAt
	}
	if start == l.checkpointAt {
		return nil
	}
	if l.maxRetained > 0 && start > l.maxRetained {
		return l.compactFromLocked(start)
	}
	if err := writeWALCheckpoint(l.checkpoint, start); err != nil {
		return fmt.Errorf("wal: write checkpoint: %w", err)
	}
	l.checkpointAt = start
	l.dropOffsetsLocked(start)
	return nil
}

// checkpointForLocked returns the offset at which the log must keep replaying.
// The caller passes every batch it still holds, so the minimum start among them
// is the earliest acknowledged batch. Frames before it are either already
// flushed or belong to an Append that has not returned yet; neither may be
// replayed after a successful flush.
func (l *commitLog[T]) checkpointForLocked(ids []string) (int64, bool) {
	if len(ids) == 0 {
		return l.size, true
	}
	start := int64(-1)
	for _, id := range ids {
		offset, ok := l.offsets[id]
		if !ok {
			return 0, false
		}
		if start < 0 || offset.start < start {
			start = offset.start
		}
	}
	if start < 0 {
		start = l.size
	}
	return start, true
}

// compactFromLocked copies the live tail [start, size) into a fresh file. It
// copies raw frames, so appends that are already in the WAL but not yet in the
// caller's batch list are preserved.
func (l *commitLog[T]) compactFromLocked(start int64) error {
	if start <= 0 {
		return nil
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	src, err := os.Open(l.path)
	if err != nil {
		return fmt.Errorf("wal: open log for compaction: %w", err)
	}
	defer src.Close()
	if _, err := src.Seek(start, io.SeekStart); err != nil {
		return fmt.Errorf("wal: seek log for compaction: %w", err)
	}

	tmpPath := l.path + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("wal: open temp file for compaction: %w", err)
	}
	copied, err := io.Copy(dst, src)
	if err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: copy live tail: %w", err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: sync temp file: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: close temp file: %w", err)
	}
	if err := writeWALCheckpoint(l.checkpoint, 0); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: reset checkpoint: %w", err)
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: replace log: %w", err)
	}

	offsets := make(map[string]batchOffset, len(l.offsets))
	for id, offset := range l.offsets {
		if offset.end <= start {
			continue
		}
		offsets[id] = batchOffset{start: offset.start - start, end: offset.end - start}
	}
	l.offsets = offsets
	l.size = copied
	l.checkpointAt = 0
	return nil
}

func (l *commitLog[T]) dropOffsetsLocked(checkpoint int64) {
	for id, offset := range l.offsets {
		if offset.end <= checkpoint {
			delete(l.offsets, id)
		}
	}
}

// rewriteLocked writes the live batches into a fresh file and resets the
// checkpoint. The checkpoint is reset before the rename so a crash can only
// replay old data, never skip new data.
func (l *commitLog[T]) rewriteLocked(ids []string, payloads [][]byte) error {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}

	tmpPath := l.path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("wal: open temp file: %w", err)
	}
	writer := bufio.NewWriter(file)
	offsets := make(map[string]batchOffset, len(ids))
	var size int64
	for i, payload := range payloads {
		start := size
		var frame [4]byte
		binary.LittleEndian.PutUint32(frame[:], uint32(len(payload)))
		if _, err := writer.Write(frame[:]); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("wal: write frame size: %w", err)
		}
		if _, err := writer.Write(payload); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("wal: write frame: %w", err)
		}
		size += int64(len(frame) + len(payload))
		if i < len(ids) && ids[i] != "" {
			offsets[ids[i]] = batchOffset{start: start, end: size}
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: flush temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: sync temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: close temp file: %w", err)
	}
	// Reset the checkpoint before the rename. If we crash after this write but
	// before the rename, replay starts at zero (duplicates, not data loss).
	if err := writeWALCheckpoint(l.checkpoint, 0); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: reset checkpoint: %w", err)
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("wal: replace log: %w", err)
	}
	l.checkpointAt = 0
	l.size = size
	l.offsets = offsets
	return nil
}

func (l *commitLog[T]) iterate(yield func(T) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("wal: open log: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("wal: stat log: %w", err)
	}
	position := l.checkpointAt
	if position > info.Size() {
		// A stale/corrupt checkpoint must not hide data.
		position = 0
	}
	if _, err := file.Seek(position, io.SeekStart); err != nil {
		return fmt.Errorf("wal: seek log: %w", err)
	}

	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		frameStart := position
		var size uint32
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("wal: read frame size: %w", err)
		}

		payload := make([]byte, int(size))
		if _, err := io.ReadFull(reader, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("wal: read frame: %w", err)
		}
		position = frameStart + 4 + int64(len(payload))

		var value T
		if err := json.Unmarshal(payload, &value); err != nil {
			continue
		}
		if id := l.idOf(value); id != "" {
			l.offsets[id] = batchOffset{start: frameStart, end: position}
		}
		if err := yield(value); err != nil {
			return err
		}
	}
}

func (l *commitLog[T]) readAll() ([]T, error) {
	var out []T
	err := l.iterate(func(value T) error {
		out = append(out, value)
		return nil
	})
	return out, err
}
