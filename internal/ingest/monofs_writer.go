package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	monofsstore "github.com/rydzu/ainfra/doctor/internal/store/monofs"
)

// MonoFSWriter implements Flusher by forwarding all telemetry signals to the
// monofs logengine via gRPC instead of writing packager archives to object storage.
type MonoFSWriter struct {
	client *monofsstore.LogEngineClient
}

// NewMonoFSWriter creates a Flusher backed by the monofs logengine at routerAddr.
func NewMonoFSWriter(ctx context.Context, routerAddr string) (*MonoFSWriter, error) {
	if routerAddr == "" {
		return nil, fmt.Errorf("monofs router address must be configured (DOCTOR_MONOFS_LOGENGINE_ADDR)")
	}
	client, err := monofsstore.NewLogEngineClient(ctx, routerAddr)
	if err != nil {
		return nil, fmt.Errorf("create logengine client: %w", err)
	}
	return &MonoFSWriter{client: client}, nil
}

// Close tears down the underlying gRPC connection.
func (m *MonoFSWriter) Close() error {
	return m.client.Close()
}

func (m *MonoFSWriter) WriteTraces(ctx context.Context, records []segment.TraceRecord) error {
	if len(records) == 0 {
		return nil
	}
	return m.client.IngestTraces(ctx, newChunkID(), records)
}

func (m *MonoFSWriter) WriteLogs(ctx context.Context, records []segment.LogRecord) error {
	if len(records) == 0 {
		return nil
	}
	return m.client.IngestLogs(ctx, newChunkID(), records)
}

func (m *MonoFSWriter) WriteMetrics(ctx context.Context, records []segment.MetricPointRecord) error {
	if len(records) == 0 {
		return nil
	}
	return m.client.IngestMetrics(ctx, newChunkID(), records)
}

// WriteGuardianEvents is a no-op for the monofs backend; guardian events are
// not stored in the logengine.
func (m *MonoFSWriter) WriteGuardianEvents(_ context.Context, _ []segment.GuardianEvent) error {
	return nil
}

func newChunkID() string {
	return fmt.Sprintf("chunk-%s-%d", segment.NewID(), time.Now().UnixNano())
}
