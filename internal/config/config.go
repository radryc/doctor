package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type PrivacyConfig struct {
	HMACKey string
}

type WALConfig struct {
	Enabled bool
	Dir     string
}

type PipelineConfig struct {
	Enabled   bool
	QueueSize int
	Workers   int
}

type TraceBufferConfig struct {
	Enabled         bool
	WALDir          string
	FlushInterval   time.Duration
	MaxRecords      int
	MaxBytes        int
	MaxPendingBytes int
}

type LogBufferConfig struct {
	Enabled         bool
	WALDir          string
	FlushInterval   time.Duration
	MaxBytes        int
	MaxPendingBytes int
}

type MetricBufferConfig struct {
	Enabled         bool
	WALDir          string
	FlushInterval   time.Duration
	MaxBytes        int
	MaxPendingBytes int
}

type CostConfig struct {
	MaxPendingSegments  int64
	MaxSegmentsPerHour  int64
	MaxTotalObjectBytes int64
}

type IngestConfig struct {
	DefaultTenant       string
	TenantHeader        string
	GRPCAddr            string
	HTTPAddr            string
	DebugAddr           string
	MonoFSLogEngineAddr string
	TraceBuffer         TraceBufferConfig
	LogBuffer           LogBufferConfig
	MetricBuffer        MetricBufferConfig
	Privacy             PrivacyConfig
	WAL                 WALConfig
	Pipeline            PipelineConfig
	Cost                CostConfig
}

type QueryConfig struct {
	DefaultTenant       string
	TenantHeader        string
	HTTPAddr            string
	GuardianURL         string
	GuardianAPIURL      string
	DefaultPartition    string
	MonoFSLogEngineAddr string
}

func LoadIngest(service string) (IngestConfig, error) {
	cfg := IngestConfig{
		DefaultTenant:       envString("DOCTOR_DEFAULT_TENANT", "default"),
		TenantHeader:        envString("DOCTOR_TENANT_HEADER", "X-Doctor-Tenant"),
		GRPCAddr:            envString("DOCTOR_GRPC_ADDR", ":4317"),
		HTTPAddr:            envString("DOCTOR_HTTP_ADDR", ":4318"),
		DebugAddr:           envString("DOCTOR_DEBUG_ADDR", ":6060"),
		MonoFSLogEngineAddr: envString("DOCTOR_MONOFS_LOGENGINE_ADDR", ""),
		TraceBuffer: TraceBufferConfig{
			Enabled:         envBool("DOCTOR_TRACE_BUFFER_ENABLED", true),
			WALDir:          envString("DOCTOR_TRACE_BUFFER_WAL_DIR", ".doctor/trace-buffer"),
			FlushInterval:   envDuration("DOCTOR_TRACE_BUFFER_FLUSH_INTERVAL", 5*time.Second),
			MaxRecords:      int(envInt64("DOCTOR_TRACE_BUFFER_MAX_RECORDS", 4096)),
			MaxBytes:        int(envInt64("DOCTOR_TRACE_BUFFER_MAX_BYTES", 4<<20)),
			MaxPendingBytes: int(envInt64("DOCTOR_TRACE_MAX_PENDING_BYTES", 0)), // 0 → 3×MaxBytes default
		},
		LogBuffer: LogBufferConfig{
			Enabled:         envBool("DOCTOR_LOG_BUFFER_ENABLED", true),
			WALDir:          envString("DOCTOR_LOG_BUFFER_WAL_DIR", ".doctor/log-buffer"),
			FlushInterval:   envDuration("DOCTOR_LOG_BUFFER_FLUSH_INTERVAL", 5*time.Second),
			MaxBytes:        int(envInt64("DOCTOR_LOG_BUFFER_MAX_BYTES", 4<<20)),
			MaxPendingBytes: int(envInt64("DOCTOR_LOG_MAX_PENDING_BYTES", 0)),
		},
		MetricBuffer: MetricBufferConfig{
			Enabled:         envBool("DOCTOR_METRIC_BUFFER_ENABLED", true),
			WALDir:          envString("DOCTOR_METRIC_BUFFER_WAL_DIR", ".doctor/metric-buffer"),
			FlushInterval:   envDuration("DOCTOR_METRIC_BUFFER_FLUSH_INTERVAL", 5*time.Second),
			MaxBytes:        int(envInt64("DOCTOR_METRIC_BUFFER_MAX_BYTES", 4<<20)),
			MaxPendingBytes: int(envInt64("DOCTOR_METRIC_MAX_PENDING_BYTES", 0)),
		},
		Privacy: PrivacyConfig{
			HMACKey: envString("DOCTOR_PRIVACY_HMAC_KEY", ""),
		},
		WAL: WALConfig{
			Enabled: envBool("DOCTOR_WAL_ENABLED", false),
			Dir:     envString("DOCTOR_WAL_DIR", ".doctor/wal"),
		},
		Pipeline: PipelineConfig{
			Enabled:   envBool("DOCTOR_PIPELINE_ENABLED", true),
			QueueSize: int(envInt64("DOCTOR_PIPELINE_QUEUE_SIZE", 1000)),
			Workers:   int(envInt64("DOCTOR_PIPELINE_WORKERS", 4)),
		},
		Cost: CostConfig{
			MaxPendingSegments:  envInt64("DOCTOR_COST_MAX_SEGMENTS", 0),
			MaxSegmentsPerHour:  envInt64("DOCTOR_COST_MAX_SEGMENTS_PER_HOUR", 0),
			MaxTotalObjectBytes: envInt64("DOCTOR_COST_MAX_TOTAL_BYTES", 0),
		},
	}
	if strings.TrimSpace(cfg.Privacy.HMACKey) == "" {
		return IngestConfig{}, errors.New("DOCTOR_PRIVACY_HMAC_KEY is required")
	}
	return cfg, nil
}

func LoadQuery(service string) QueryConfig {
	return QueryConfig{
		DefaultTenant:       envString("DOCTOR_DEFAULT_TENANT", "default"),
		TenantHeader:        envString("DOCTOR_TENANT_HEADER", "X-Doctor-Tenant"),
		HTTPAddr:            envString("DOCTOR_QUERY_HTTP_ADDR", envString("DOCTOR_HTTP_ADDR", ":18080")),
		GuardianURL:         envString("DOCTOR_GUARDIAN_UI_URL", ""),
		GuardianAPIURL:      envString("DOCTOR_GUARDIAN_API_URL", envString("DOCTOR_GUARDIAN_UI_URL", "")),
		DefaultPartition:    envString("DOCTOR_DEFAULT_PARTITION", ""),
		MonoFSLogEngineAddr: envString("DOCTOR_MONOFS_LOGENGINE_ADDR", ""),
	}
}

func envString(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err == nil {
		return parsed
	}
	if hours, intErr := strconv.Atoi(value); intErr == nil {
		return time.Duration(hours) * time.Hour
	}
	return fallback
}
