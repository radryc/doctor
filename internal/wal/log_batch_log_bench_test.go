package wal

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/radryc/doctor/internal/segment"
)

func makeBenchLogRecords(count int) []segment.LogRecord {
	records := make([]segment.LogRecord, count)
	body := strings.Repeat("x", 200)
	for i := range records {
		records[i] = segment.LogRecord{
			Tenant:  "default",
			Service: "bench",
			Body:    body,
		}
	}
	return records
}

func makeBenchLogBatches(batches, recordsPerBatch int) []LogBatch {
	out := make([]LogBatch, batches)
	for i := range out {
		out[i] = LogBatch{
			ID:      fmt.Sprintf("batch-%d", i),
			Records: makeBenchLogRecords(recordsPerBatch),
		}
	}
	return out
}

// benchLogBatchLog uses a disk-backed directory when DOCTOR_BENCH_DISK_DIR is
// set. b.TempDir() may be tmpfs, where fsync does not hit a real device.
func benchLogBatchLog(b *testing.B) *LogBatchLog {
	b.Helper()
	dir := os.Getenv("DOCTOR_BENCH_DISK_DIR")
	if dir != "" {
		temp, err := os.MkdirTemp(dir, "wal-bench-")
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = os.RemoveAll(temp) })
		dir = temp
	} else {
		dir = b.TempDir()
	}
	log, err := NewLogBatchLog(filepath.Join(dir, "log-wal"))
	if err != nil {
		b.Fatal(err)
	}
	return log
}

// BenchmarkDiskSync establishes the filesystem fsync floor that WAL appends
// inherit. Compare it with BenchmarkLogBatchLogAppend/records=1.
func BenchmarkDiskSync(b *testing.B) {
	dir := os.Getenv("DOCTOR_BENCH_DISK_DIR")
	if dir == "" {
		dir = b.TempDir()
	}
	file, err := os.OpenFile(filepath.Join(dir, "sync-bench"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = file.Close() })
	payload := make([]byte, 4<<10)
	for i := range payload {
		payload[i] = byte(i)
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := file.Write(payload); err != nil {
			b.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWALAppendOpenCloseSync models the previous append implementation:
// open, write, fsync, close for every batch.
func BenchmarkWALAppendOpenCloseSync(b *testing.B) {
	dir := os.Getenv("DOCTOR_BENCH_DISK_DIR")
	if dir == "" {
		dir = b.TempDir()
	}
	path := filepath.Join(dir, "open-close-bench.log")
	payload := makeBenchLogRecords(1)
	data, _ := json.Marshal(LogBatch{ID: "x", Records: payload})
	var frame [4]byte
	binary.LittleEndian.PutUint32(frame[:], uint32(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := file.Write(frame[:]); err != nil {
			b.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			b.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			b.Fatal(err)
		}
		if err := file.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWALAppendKeepOpenSync models the new append path without the group
// coordination: one open file, write, fsync per batch.
func BenchmarkWALAppendKeepOpenSync(b *testing.B) {
	dir := os.Getenv("DOCTOR_BENCH_DISK_DIR")
	if dir == "" {
		dir = b.TempDir()
	}
	path := filepath.Join(dir, "keep-open-bench.log")
	payload := makeBenchLogRecords(1)
	data, _ := json.Marshal(LogBatch{ID: "x", Records: payload})
	var frame [4]byte
	binary.LittleEndian.PutUint32(frame[:], uint32(len(data)))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = file.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := file.Write(frame[:]); err != nil {
			b.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			b.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWALAppendParallelOldMutex models the previous implementation under
// concurrency: every append serializes on one mutex around open/write/fsync/close.
func BenchmarkWALAppendParallelOldMutex(b *testing.B) {
	dir := os.Getenv("DOCTOR_BENCH_DISK_DIR")
	if dir == "" {
		dir = b.TempDir()
	}
	path := filepath.Join(dir, "old-parallel-bench.log")
	batch := LogBatch{ID: "x", Records: makeBenchLogRecords(100)}
	var mu sync.Mutex
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			data, _ := json.Marshal(batch)
			var frame [4]byte
			binary.LittleEndian.PutUint32(frame[:], uint32(len(data)))
			file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				_, err = file.Write(frame[:])
			}
			if err == nil {
				_, err = file.Write(data)
			}
			if err == nil {
				err = file.Sync()
			}
			if file != nil {
				_ = file.Close()
			}
			mu.Unlock()
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkWALAppendParallelOldMutexKeepOpen is the fairest baseline for group
// commit: same open file and mutex as the old code, but no fsync batching.
func BenchmarkWALAppendParallelOldMutexKeepOpen(b *testing.B) {
	dir := os.Getenv("DOCTOR_BENCH_DISK_DIR")
	if dir == "" {
		dir = b.TempDir()
	}
	path := filepath.Join(dir, "old-parallel-keep-open-bench.log")
	batch := LogBatch{ID: "x", Records: makeBenchLogRecords(100)}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = file.Close() })
	var mu sync.Mutex
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			data, _ := json.Marshal(batch)
			var frame [4]byte
			binary.LittleEndian.PutUint32(frame[:], uint32(len(data)))
			_, err := file.Write(frame[:])
			if err == nil {
				_, err = file.Write(data)
			}
			if err == nil {
				err = file.Sync()
			}
			mu.Unlock()
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkLogBatchLogAppend(b *testing.B) {
	for _, records := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("records=%d", records), func(b *testing.B) {
			log := benchLogBatchLog(b)
			batch := LogBatch{Records: makeBenchLogRecords(records)}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := log.Append(batch); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkLogBatchLogAppendParallel(b *testing.B) {
	log := benchLogBatchLog(b)
	batch := LogBatch{Records: makeBenchLogRecords(100)}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := log.Append(batch); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkLogBatchLogRetireFastPath measures retiring already-flushed
// batches with a checkpoint write instead of rewriting the WAL. The offsets
// are pre-populated to model a file with a 32 MiB flushed prefix.
func BenchmarkLogBatchLogRetireFastPath(b *testing.B) {
	for _, batches := range []int{100, 1000} {
		b.Run(fmt.Sprintf("batches=%d", batches), func(b *testing.B) {
			dir := b.TempDir()
			log := newCommitLog[LogBatch](filepath.Join(dir, "log-wal"), func(batch LogBatch) string {
				return batch.ID
			})
			ids := make([]string, batches)
			payloads := make([][]byte, batches)
			var offset int64 = 32 << 20
			for i := range payloads {
				data, err := json.Marshal(LogBatch{ID: fmt.Sprintf("batch-%d", i), Records: makeBenchLogRecords(100)})
				if err != nil {
					b.Fatal(err)
				}
				ids[i] = fmt.Sprintf("batch-%d", i)
				payloads[i] = data
				start := offset
				offset += int64(4 + len(data))
				log.offsets[ids[i]] = batchOffset{start: start, end: offset}
			}
			log.size = offset

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				log.mu.Lock()
				log.checkpointAt = 0
				log.mu.Unlock()
				if err := log.retire(ids); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkLogBatchLogReplace(b *testing.B) {
	for _, batches := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("batches=%d", batches), func(b *testing.B) {
			log := benchLogBatchLog(b)
			pending := makeBenchLogBatches(batches, 100)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := log.Replace(pending); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
