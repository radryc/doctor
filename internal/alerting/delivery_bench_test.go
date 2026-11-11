package alerting

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// BenchmarkDispatchOnce measures delivery of a batch of independent outbox
// records. The fake webhook adds a fixed latency, so a serial dispatcher costs
// N*latency while the bounded worker pool costs ~latency.
func BenchmarkDispatchOnce(b *testing.B) {
	for _, recordCount := range []int{8, 16, 32} {
		b.Run(fmt.Sprintf("records=%d", recordCount), func(b *testing.B) {
			const latency = 2 * time.Millisecond
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(latency)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			ctx := context.Background()
			store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
			if err := store.PutReceiverConfig(ctx, ReceiverConfig{Receivers: []Receiver{{
				Name: "bench", Type: "webhook", URL: server.URL,
			}}}); err != nil {
				b.Fatal(err)
			}
			dispatcher := &Dispatcher{Store: store}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				now := time.Now().UTC()
				for r := 0; r < recordCount; r++ {
					fingerprint := fmt.Sprintf("alert-%d-%d", i, r)
					alert := Alert{
						Fingerprint: fingerprint,
						ID:          fingerprint,
						Source:      "bench",
						Tenant:      "default",
						Partition:   "bench",
						Status:      StatusFiring,
						Severity:    "warning",
						Labels:      map[string]string{"alertname": "Bench"},
					}
					record := OutboxRecord{
						ID:          fingerprint,
						Fingerprint: fingerprint,
						Tenant:      "default",
						Partition:   "bench",
						Channel:     "bench",
						Status:      "pending",
						NextAttempt: now,
						CreatedAt:   now,
						UpdatedAt:   now,
					}
					if err := store.SaveAlert(ctx, alert, []OutboxRecord{record}); err != nil {
						b.Fatal(err)
					}
				}
				if err := dispatcher.DispatchOnce(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
