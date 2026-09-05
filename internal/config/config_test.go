package config

import (
	"testing"
	"time"
)

func TestLoadIngestUsesLowLatencyBufferDefaults(t *testing.T) {
	t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "test-hmac-key")
	t.Setenv("DOCTOR_TRACE_BUFFER_FLUSH_INTERVAL", "")
	t.Setenv("DOCTOR_LOG_BUFFER_FLUSH_INTERVAL", "")
	t.Setenv("DOCTOR_METRIC_BUFFER_FLUSH_INTERVAL", "")
	t.Setenv("DOCTOR_TRACE_BUFFER_MAX_RECORDS", "")
	t.Setenv("DOCTOR_TRACE_BUFFER_MAX_BYTES", "")
	t.Setenv("DOCTOR_LOG_BUFFER_MAX_BYTES", "")
	t.Setenv("DOCTOR_METRIC_BUFFER_MAX_BYTES", "")

	cfg, err := LoadIngest("doctor-ingest")
	if err != nil {
		t.Fatalf("LoadIngest() error = %v", err)
	}

	if cfg.TraceBuffer.FlushInterval != 5*time.Second {
		t.Fatalf("trace flush interval = %v, want 5s", cfg.TraceBuffer.FlushInterval)
	}
	if cfg.LogBuffer.FlushInterval != 5*time.Second {
		t.Fatalf("log flush interval = %v, want 5s", cfg.LogBuffer.FlushInterval)
	}
	if cfg.MetricBuffer.FlushInterval != 5*time.Second {
		t.Fatalf("metric flush interval = %v, want 5s", cfg.MetricBuffer.FlushInterval)
	}
	if cfg.TraceBuffer.MaxRecords != 4096 {
		t.Fatalf("trace max records = %d, want 4096", cfg.TraceBuffer.MaxRecords)
	}
	if cfg.TraceBuffer.MaxBytes != 4<<20 {
		t.Fatalf("trace max bytes = %d, want %d", cfg.TraceBuffer.MaxBytes, 4<<20)
	}
	if cfg.LogBuffer.MaxBytes != 4<<20 {
		t.Fatalf("log max bytes = %d, want %d", cfg.LogBuffer.MaxBytes, 4<<20)
	}
	if cfg.MetricBuffer.MaxBytes != 4<<20 {
		t.Fatalf("metric max bytes = %d, want %d", cfg.MetricBuffer.MaxBytes, 4<<20)
	}
}
