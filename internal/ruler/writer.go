package ruler

import (
	"context"
	"fmt"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	monofsstore "github.com/rydzu/ainfra/doctor/internal/store/monofs"
)

// LogEngineWriter writes derived samples through the MonoFS logengine client.
type LogEngineWriter struct {
	Client *monofsstore.LogEngineClient
	Prefix string
}

// NewLogEngineWriter builds a writer over a logengine client.
func NewLogEngineWriter(client *monofsstore.LogEngineClient) *LogEngineWriter {
	return &LogEngineWriter{Client: client, Prefix: "rules"}
}

// WriteMetrics implements MetricWriter.
func (w *LogEngineWriter) WriteMetrics(ctx context.Context, records []segment.MetricPointRecord) error {
	if w.Client == nil {
		return fmt.Errorf("ruler: logengine writer has no client")
	}
	if len(records) == 0 {
		return nil
	}
	prefix := w.Prefix
	if prefix == "" {
		prefix = "rules"
	}
	chunkID := fmt.Sprintf("%s-%d-%d", prefix, time.Now().UTC().UnixNano(), len(records))
	return w.Client.IngestMetrics(ctx, chunkID, records)
}
