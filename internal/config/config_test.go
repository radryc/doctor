package config

import (
	"testing"
	"time"
)

func TestLoadRulesEnabledByDefault(t *testing.T) {
	t.Setenv("DOCTOR_RULES_ENABLED", "")

	cfg := LoadRules("doctor-rules")
	if !cfg.Enabled {
		t.Fatal("LoadRules() Enabled = false, want true by default")
	}
	if cfg.HTTPAddr != ":18081" {
		t.Fatalf("HTTPAddr = %q, want :18081", cfg.HTTPAddr)
	}
}

func TestLoadRulesHonorsDisableSwitch(t *testing.T) {
	t.Setenv("DOCTOR_RULES_ENABLED", "false")

	cfg := LoadRules("doctor-rules")
	if cfg.Enabled {
		t.Fatal("LoadRules() Enabled = true, want false when DOCTOR_RULES_ENABLED=false")
	}
}

func TestLoadIngestRequiresAuthToken(t *testing.T) {
	t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "test-hmac-key")
	t.Setenv("DOCTOR_INGEST_AUTH_TOKEN", "")
	if _, err := LoadIngest("doctor-ingest"); err == nil {
		t.Fatal("LoadIngest() succeeded without DOCTOR_INGEST_AUTH_TOKEN")
	}
}

func TestLoadIngestUsesLowLatencyBufferDefaults(t *testing.T) {
	t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "test-hmac-key")
	t.Setenv("DOCTOR_INGEST_AUTH_TOKEN", "test-ingest-token")
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
	if len(cfg.AllowedTenants) != 1 || cfg.AllowedTenants[0] != "default" {
		t.Fatalf("allowed tenants = %#v, want [default]", cfg.AllowedTenants)
	}
}
