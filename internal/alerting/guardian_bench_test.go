package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
)

// countingFileStore counts the MonoFS-shaped calls made by the Guardian
// reconciler, optionally adding a fixed latency to model a real RPC.
type countingFileStore struct {
	*MemoryStore
	latency  time.Duration
	reads    atomic.Int64
	lists    atomic.Int64
	versions atomic.Int64
	upserts  atomic.Int64
	deletes  atomic.Int64
}

func (s *countingFileStore) sleep() {
	if s.latency > 0 {
		time.Sleep(s.latency)
	}
}

func (s *countingFileStore) ReadFile(ctx context.Context, logicalPath string) ([]byte, error) {
	s.reads.Add(1)
	s.sleep()
	return s.MemoryStore.ReadFile(ctx, logicalPath)
}

func (s *countingFileStore) ListDir(ctx context.Context, logicalDir string) ([]doctorstore.DirEntry, error) {
	s.lists.Add(1)
	s.sleep()
	return s.MemoryStore.ListDir(ctx, logicalDir)
}

func (s *countingFileStore) ListVersions(ctx context.Context, token, logicalPath string) ([]doctorstore.FileVersion, error) {
	s.versions.Add(1)
	s.sleep()
	return s.MemoryStore.ListVersions(ctx, token, logicalPath)
}

func (s *countingFileStore) UpsertPaths(ctx context.Context, token string, writes []doctorstore.PathWrite, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error) {
	s.upserts.Add(1)
	s.sleep()
	return s.MemoryStore.UpsertPaths(ctx, token, writes, mutation)
}

func (s *countingFileStore) DeletePaths(ctx context.Context, token string, deletes []doctorstore.PathDelete, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error) {
	s.deletes.Add(1)
	s.sleep()
	return s.MemoryStore.DeletePaths(ctx, token, deletes, mutation)
}

func benchmarkGuardianPoll(b *testing.B, partitions int, unhealthy bool, latency time.Duration) {
	b.Helper()

	summaries := make([]GuardianPartitionSummary, partitions)
	for i := range summaries {
		status := "healthy"
		if unhealthy {
			status = "error"
		}
		summaries[i] = GuardianPartitionSummary{
			Name:   fmt.Sprintf("part-%05d", i),
			Status: status,
			Health: status,
			Errors: nil,
		}
		if unhealthy {
			summaries[i].Errors = []string{"bench failure"}
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(summaries)
	}))
	defer server.Close()

	files := &countingFileStore{MemoryStore: NewMemoryStore(), latency: latency}
	store := NewStore(files, "token", "/doctor/v1/alerts")
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	service := &Service{Store: store, DefaultTenant: "default", Now: func() time.Time { return now }}
	source := &GuardianSource{
		BaseURL: server.URL,
		Tenant:  "default",
		Service: service,
		Store:   store,
		HTTP:    server.Client(),
		Now:     func() time.Time { return now },
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := source.PollOnce(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(files.reads.Load())/float64(b.N), "reads/op")
	b.ReportMetric(float64(files.lists.Load())/float64(b.N), "listdir/op")
	b.ReportMetric(float64(files.versions.Load())/float64(b.N), "versions/op")
	b.ReportMetric(float64(files.upserts.Load())/float64(b.N), "upserts/op")
}

func BenchmarkGuardianPollHealthy(b *testing.B) {
	for _, partitions := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("partitions=%d", partitions), func(b *testing.B) {
			benchmarkGuardianPoll(b, partitions, false, 0)
		})
	}
}

func BenchmarkGuardianPollUnhealthy(b *testing.B) {
	for _, partitions := range []int{100, 1000} {
		b.Run(fmt.Sprintf("partitions=%d", partitions), func(b *testing.B) {
			benchmarkGuardianPoll(b, partitions, true, 0)
		})
	}
}

// BenchmarkGuardianPollHealthyRPC models each FileStore call as an RPC. The
// absolute time is dominated by the injected latency; the reads/op metric is
// the signal to optimize against.
func BenchmarkGuardianPollHealthyRPC(b *testing.B) {
	benchmarkGuardianPoll(b, 10, false, 100*time.Microsecond)
}
