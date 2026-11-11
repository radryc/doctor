package alerting

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkOutboxScan compares the dispatcher's pending-index scan with the
// legacy tenant/partition walk. The store has 1,000 partitions with no pending
// records, which is the steady state once deliveries have completed.
func BenchmarkOutboxScan(b *testing.B) {
	ctx := context.Background()
	mem := NewMemoryStore()
	store := NewStore(mem, "token", "/doctor/v1/alerts")
	for i := 0; i < 1000; i++ {
		// A non-.json file creates the partition directory without creating a
		// deliverable record.
		mem.files[fmt.Sprintf("%s/outbox/default/part-%04d/.keep", store.Root, i)] = nil
	}

	b.Run("legacy-full-walk", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := store.ListOutboxAll(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("pending-index", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := store.ListOutboxPending(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}
