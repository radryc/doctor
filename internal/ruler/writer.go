package ruler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/radryc/doctor/internal/segment"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
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
	// Derive the chunk ID from the record content. Retries, backfills, and a
	// scheduler restart that replays the same tick then address the same
	// chunk instead of appending duplicate samples.
	chunkID, err := rulerChunkID(prefix, records)
	if err != nil {
		return err
	}
	return w.Client.IngestMetrics(ctx, chunkID, records)
}

func rulerChunkID(prefix string, records []segment.MetricPointRecord) (string, error) {
	encoded, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("ruler: encode metrics for chunk id: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(sum[:16])), nil
}
