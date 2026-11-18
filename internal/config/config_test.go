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

func TestLoadSentinelFCBIsOptional(t *testing.T) {
	t.Setenv("DOCTOR_SENTINEL_FCB_BACKEND_URL", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_ENABLED", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_WEBHOOK_SECRET", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_ACK_SECRET", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_DEFAULT_TOPIC", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_TIMEOUT", "")
	t.Setenv("DOCTOR_FCB_BACKEND_URL", "")
	t.Setenv("DOCTOR_FCB_ENABLED", "")
	t.Setenv("DOCTOR_FCB_WEBHOOK_SECRET", "")
	t.Setenv("DOCTOR_FCB_ACK_SECRET", "")
	t.Setenv("DOCTOR_FCB_DEFAULT_TOPIC", "")
	t.Setenv("DOCTOR_FCB_TIMEOUT", "")

	cfg := LoadSentinel("doctor-sentinel")
	if cfg.FCBEnabled {
		t.Fatal("FCBEnabled = true, want false when no backend URL is configured")
	}
	if cfg.FCBDefaultTopic != "" {
		t.Fatalf("FCBDefaultTopic = %q, want empty", cfg.FCBDefaultTopic)
	}
	if cfg.FCBAckSecret != "" {
		t.Fatalf("FCBAckSecret = %q, want empty", cfg.FCBAckSecret)
	}
	if cfg.FCBTimeout != 10*time.Second {
		t.Fatalf("FCBTimeout = %v", cfg.FCBTimeout)
	}
}

func TestLoadSentinelFCBEnablesWithBackendURL(t *testing.T) {
	t.Setenv("DOCTOR_SENTINEL_FCB_BACKEND_URL", "http://doctor-fcm-bridge:8080/webhook")
	t.Setenv("DOCTOR_SENTINEL_FCB_ENABLED", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_WEBHOOK_SECRET", "shared-secret")
	t.Setenv("DOCTOR_SENTINEL_FCB_ACK_SECRET", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_DEFAULT_TOPIC", "doctor-alerts")
	t.Setenv("DOCTOR_SENTINEL_FCB_TIMEOUT", "3s")

	cfg := LoadSentinel("doctor-sentinel")
	if !cfg.FCBEnabled {
		t.Fatal("FCBEnabled = false, want true when a backend URL is configured")
	}
	if cfg.FCBBackendURL != "http://doctor-fcm-bridge:8080/webhook" {
		t.Fatalf("FCBBackendURL = %q", cfg.FCBBackendURL)
	}
	if cfg.FCBWebhookSecret != "shared-secret" {
		t.Fatalf("FCBWebhookSecret = %q", cfg.FCBWebhookSecret)
	}
	if cfg.FCBAckSecret != "shared-secret" {
		t.Fatalf("FCBAckSecret = %q, want webhook secret fallback", cfg.FCBAckSecret)
	}
	if cfg.FCBTimeout != 3*time.Second {
		t.Fatalf("FCBTimeout = %v", cfg.FCBTimeout)
	}
}

func TestLoadSentinelFCBAckSecretOverride(t *testing.T) {
	t.Setenv("DOCTOR_SENTINEL_FCB_WEBHOOK_SECRET", "shared-secret")
	t.Setenv("DOCTOR_SENTINEL_FCB_ACK_SECRET", "ack-secret")

	cfg := LoadSentinel("doctor-sentinel")
	if cfg.FCBAckSecret != "ack-secret" {
		t.Fatalf("FCBAckSecret = %q, want explicit override", cfg.FCBAckSecret)
	}
}

func TestLoadSentinelFCBAliasEnv(t *testing.T) {
	t.Setenv("DOCTOR_SENTINEL_FCB_BACKEND_URL", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_ENABLED", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_WEBHOOK_SECRET", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_DEFAULT_TOPIC", "")
	t.Setenv("DOCTOR_SENTINEL_FCB_TIMEOUT", "")
	t.Setenv("DOCTOR_FCB_ENABLED", "")
	t.Setenv("DOCTOR_FCB_DEFAULT_TOPIC", "")
	t.Setenv("DOCTOR_FCB_TIMEOUT", "")
	t.Setenv("DOCTOR_FCB_BACKEND_URL", "http://doctor-fcm-bridge:8080/webhook")
	t.Setenv("DOCTOR_FCB_WEBHOOK_SECRET", "alias-secret")

	cfg := LoadSentinel("doctor-sentinel")
	if !cfg.FCBEnabled || cfg.FCBBackendURL != "http://doctor-fcm-bridge:8080/webhook" {
		t.Fatalf("FCB alias config = %#v", cfg)
	}
	if cfg.FCBWebhookSecret != "alias-secret" {
		t.Fatalf("FCBWebhookSecret = %q", cfg.FCBWebhookSecret)
	}
}

func TestLoadIngestRequiresAuthToken(t *testing.T) {
	t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "test-hmac-key")
	t.Setenv("DOCTOR_INGEST_AUTH_TOKEN", "")
	if _, err := LoadIngest("doctor-ingest"); err == nil {
		t.Fatal("LoadIngest() succeeded without DOCTOR_INGEST_AUTH_TOKEN")
	}
}

func TestLoadIngestAcceptsCollectorTokenFile(t *testing.T) {
	t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "test-hmac-key")
	t.Setenv("DOCTOR_INGEST_AUTH_TOKEN", "")
	t.Setenv("DOCTOR_INGEST_TOKENS_FILE", "/etc/doctor/ingest-tokens.yaml")
	cfg, err := LoadIngest("doctor-ingest")
	if err != nil {
		t.Fatalf("LoadIngest() error = %v", err)
	}
	if cfg.AuthTokensFile != "/etc/doctor/ingest-tokens.yaml" {
		t.Fatalf("AuthTokensFile = %q", cfg.AuthTokensFile)
	}
	if len(cfg.AllowedTenants) != 0 {
		t.Fatalf("AllowedTenants = %v, want no implicit default with a token file", cfg.AllowedTenants)
	}
}

func TestLoadIngestMTLSConfiguration(t *testing.T) {
	t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "test-hmac-key")
	t.Setenv("DOCTOR_INGEST_AUTH_TOKEN", "test-ingest-token")
	t.Setenv("DOCTOR_INGEST_MTLS_MODE", "require")
	t.Setenv("DOCTOR_INGEST_MTLS_CA_FILE", "/etc/doctor/collectors-ca.pem")
	t.Setenv("DOCTOR_HTTP_MTLS_MODE", "off")
	t.Setenv("DOCTOR_HTTP_MTLS_CA_FILE", "")

	cfg, err := LoadIngest("doctor-ingest")
	if err != nil {
		t.Fatalf("LoadIngest: %v", err)
	}
	if cfg.GRPCMTLSMode != "require" || cfg.GRPCMTLSCAFile != "/etc/doctor/collectors-ca.pem" {
		t.Fatalf("grpc mTLS = %q / %q", cfg.GRPCMTLSMode, cfg.GRPCMTLSCAFile)
	}
	if cfg.HTTPMTLSMode != "off" {
		t.Fatalf("http mTLS = %q, want explicit override", cfg.HTTPMTLSMode)
	}

	t.Setenv("DOCTOR_INGEST_MTLS_CA_FILE", "")
	if _, err := LoadIngest("doctor-ingest"); err == nil {
		t.Fatal("expected require mode without a CA file to fail")
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
