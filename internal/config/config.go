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
	RulesURL            string
	AWSEnabled          bool
	AWSBackendURL       string
	AWSBackendToken     string

	FederationEnabled              bool
	FederationCatalogFile          string
	FederationSnapshotStore        string
	FederationSnapshotRoot         string
	FederationSnapshotTenant       string
	FederationSnapshotShardTargets int
	FederationTokenResolver        string
	FederationTokenCacheTTL        time.Duration
	FederationTokenBundleRoot      string
	FederationTokenShardCount      int
	FederationReloadInterval       time.Duration
	FederationStaleAfter           time.Duration
	FederationMaxFanout            int
	FederationMaxInflight          int
	FederationMaxConcurrentTargets int
	FederationMaxAttempts          int
	FederationClientPoolMax        int
	FederationClientIdleTTL        time.Duration
	FederationBreakerThreshold     int
	FederationBreakerCooldown      time.Duration
	FederationMetadataTTL          time.Duration
	FederationTimeout              time.Duration
	TraceDiscoveryCacheTTL         time.Duration
	TraceDiscoveryWindow           time.Duration

	ProxyEnabled              bool
	ProxyBackendsFile         string
	ProxyRegistryInterval     time.Duration
	ProxyMaxInflight          int
	ProxyBackendTimeout       time.Duration
	GuardianDiscoveryEnabled  bool
	GuardianDiscoveryInterval time.Duration
	GuardianToken             string

	AuthRequire       bool
	AuthExemptPaths   []string
	OIDCIssuer        string
	OIDCAudience      string
	OIDCJWKSURL       string
	OIDCClientID      string
	OIDCClientSecret  string
	OIDCRedirectURL   string
	OIDCEndSessionURL string
	OIDCAuthURL       string
	OIDCRequireLogin  bool
	OIDCScopes        []string
	SessionDir        string
	BreakGlassToken   string

	AccessResolver          string
	AccessMode              string
	AccessEnforceLogs       bool
	AccessEnforceTraces     bool
	AccessTeamMapping       string
	AccessGrantsPath        string
	AccessOwnersRoot        string
	AccessCacheTTL          time.Duration
	AccessStaticAllow       string
	AccessStaticAllowGroups string
	AccessStaticDeny        string
	AccessTrustTenantHeader bool

	// Repo-managed access policy: per-partition filtering for logs and traces.
	AccessPolicyURL      string
	AccessPolicyFile     string
	AccessPolicyToken    string
	AccessPolicyInterval time.Duration
	MonoFSFileToken      string
	MonoFSClientID       string

	// Dashboards
	DashboardsStoreRoot string
}

// RulesConfig holds doctor-rules settings.
type RulesConfig struct {
	Enabled             bool
	HTTPAddr            string
	QueryURL            string
	QueryToken          string
	Workers             int
	EvalTimeout         time.Duration
	LeaseTTL            time.Duration
	LeaseRenew          time.Duration
	RulerID             string
	StoreRoot           string
	PollInterval        time.Duration
	MonoFSLogEngineAddr string
	MonoFSToken         string
	MonoFSClientID      string
}

// LoadRules loads the doctor-rules configuration from the environment.
func LoadRules(service string) RulesConfig {
	rulerID := envString("DOCTOR_RULES_RULER_ID", "")
	if rulerID == "" {
		if hostname, err := os.Hostname(); err == nil {
			rulerID = hostname
		} else {
			rulerID = "doctor-rules"
		}
	}
	return RulesConfig{
		Enabled:             envBool("DOCTOR_RULES_ENABLED", true),
		HTTPAddr:            envString("DOCTOR_RULES_HTTP_ADDR", ":18081"),
		QueryURL:            envString("DOCTOR_RULES_QUERY_URL", ""),
		QueryToken:          envString("DOCTOR_RULES_QUERY_TOKEN", ""),
		Workers:             int(envInt64("DOCTOR_RULES_WORKERS", 8)),
		EvalTimeout:         envDuration("DOCTOR_RULES_EVAL_TIMEOUT", 30*time.Second),
		LeaseTTL:            envDuration("DOCTOR_RULES_LEASE_TTL", 30*time.Second),
		LeaseRenew:          envDuration("DOCTOR_RULES_LEASE_RENEW", 10*time.Second),
		RulerID:             rulerID,
		StoreRoot:           envString("DOCTOR_RULES_STORE_ROOT", "/doctor/v1/rules"),
		PollInterval:        envDuration("DOCTOR_RULES_POLL_INTERVAL", 30*time.Second),
		MonoFSLogEngineAddr: envString("DOCTOR_MONOFS_LOGENGINE_ADDR", ""),
		MonoFSToken:         envString("DOCTOR_MONOFS_TOKEN", ""),
		MonoFSClientID:      envString("DOCTOR_MONOFS_CLIENT_ID", "doctor-rules"),
	}
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
		RulesURL:            envString("DOCTOR_RULES_URL", ""),
		AWSEnabled:          envBool("DOCTOR_AWS_ENABLED", true),
		AWSBackendURL:       envString("DOCTOR_AWS_BACKEND_URL", ""),
		AWSBackendToken:     envString("DOCTOR_AWS_BACKEND_TOKEN", ""),

		FederationEnabled:              envBool("DOCTOR_FEDERATION_ENABLED", false),
		FederationCatalogFile:          envString("DOCTOR_FEDERATION_CATALOG_FILE", ""),
		FederationSnapshotStore:        envString("DOCTOR_FEDERATION_SNAPSHOT_STORE", "file"),
		FederationSnapshotRoot:         envString("DOCTOR_FEDERATION_SNAPSHOT_ROOT", "/doctor/v1/federation"),
		FederationSnapshotTenant:       envString("DOCTOR_FEDERATION_SNAPSHOT_TENANT", "default"),
		FederationSnapshotShardTargets: int(envInt64("DOCTOR_FEDERATION_SNAPSHOT_SHARD_TARGETS", 500)),
		FederationTokenResolver:        envString("DOCTOR_FEDERATION_TOKEN_RESOLVER", "auto"),
		FederationTokenCacheTTL:        envDuration("DOCTOR_FEDERATION_TOKEN_CACHE_TTL", 5*time.Minute),
		FederationTokenBundleRoot:      envString("DOCTOR_FEDERATION_TOKEN_BUNDLE_ROOT", "/doctor/v1/federation/tokens"),
		FederationTokenShardCount:      int(envInt64("DOCTOR_FEDERATION_TOKEN_SHARD_COUNT", 256)),
		FederationReloadInterval:       envDuration("DOCTOR_FEDERATION_RELOAD_INTERVAL", 30*time.Second),
		FederationStaleAfter:           envDuration("DOCTOR_FEDERATION_STALE_AFTER", 0),
		FederationMaxFanout:            int(envInt64("DOCTOR_FEDERATION_MAX_FANOUT", 32)),
		FederationMaxInflight:          int(envInt64("DOCTOR_FEDERATION_MAX_INFLIGHT", 64)),
		FederationMaxConcurrentTargets: int(envInt64("DOCTOR_FEDERATION_MAX_CONCURRENT_TARGETS", 8)),
		FederationMaxAttempts:          int(envInt64("DOCTOR_FEDERATION_MAX_ATTEMPTS", 2)),
		FederationClientPoolMax:        int(envInt64("DOCTOR_FEDERATION_CLIENT_POOL_MAX", 10000)),
		FederationClientIdleTTL:        envDuration("DOCTOR_FEDERATION_CLIENT_IDLE_TTL", 15*time.Minute),
		FederationBreakerThreshold:     int(envInt64("DOCTOR_FEDERATION_BREAKER_THRESHOLD", 5)),
		FederationBreakerCooldown:      envDuration("DOCTOR_FEDERATION_BREAKER_COOLDOWN", 30*time.Second),
		FederationMetadataTTL:          envDuration("DOCTOR_FEDERATION_METADATA_TTL", time.Hour),
		FederationTimeout:              envDuration("DOCTOR_FEDERATION_TIMEOUT", 5*time.Second),
		TraceDiscoveryCacheTTL:         envDuration("DOCTOR_TRACE_DISCOVERY_CACHE_TTL", 30*time.Second),
		TraceDiscoveryWindow:           envDuration("DOCTOR_TRACE_DISCOVERY_WINDOW", time.Hour),

		ProxyEnabled:              envBool("DOCTOR_PROXY_ENABLED", true),
		ProxyBackendsFile:         envString("DOCTOR_PROXY_BACKENDS_FILE", ""),
		ProxyRegistryInterval:     envDuration("DOCTOR_PROXY_REGISTRY_INTERVAL", 30*time.Second),
		ProxyMaxInflight:          int(envInt64("DOCTOR_PROXY_MAX_INFLIGHT", 32)),
		ProxyBackendTimeout:       envDuration("DOCTOR_PROXY_BACKEND_TIMEOUT", 15*time.Second),
		GuardianDiscoveryEnabled:  envBool("DOCTOR_GUARDIAN_DISCOVERY_ENABLED", false),
		GuardianDiscoveryInterval: envDuration("DOCTOR_GUARDIAN_DISCOVERY_INTERVAL", 30*time.Second),
		GuardianToken:             envString("DOCTOR_GUARDIAN_TOKEN", ""),

		AuthRequire:       envBool("DOCTOR_AUTH_REQUIRE", false),
		AuthExemptPaths:   envCSV("DOCTOR_AUTH_EXEMPT_PATHS", nil),
		OIDCIssuer:        envString("DOCTOR_OIDC_ISSUER", ""),
		OIDCAudience:      envString("DOCTOR_OIDC_AUDIENCE", ""),
		OIDCJWKSURL:       envString("DOCTOR_OIDC_JWKS_URL", ""),
		OIDCClientID:      envString("DOCTOR_OIDC_CLIENT_ID", ""),
		OIDCClientSecret:  envString("DOCTOR_OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:   envString("DOCTOR_OIDC_REDIRECT_URL", ""),
		OIDCEndSessionURL: envString("DOCTOR_OIDC_END_SESSION_URL", ""),
		OIDCAuthURL:       envString("DOCTOR_OIDC_AUTH_URL", ""),
		OIDCRequireLogin:  envBool("DOCTOR_OIDC_REQUIRE_LOGIN", false),
		OIDCScopes:        envCSV("DOCTOR_OIDC_SCOPES", nil),
		SessionDir:        envString("DOCTOR_SESSION_DIR", ".doctor/sessions"),
		BreakGlassToken:   envString("DOCTOR_BREAK_GLASS_TOKEN", ""),

		AccessResolver:          envString("DOCTOR_ACCESS_RESOLVER", "owners"),
		AccessMode:              envString("DOCTOR_ACCESS_MODE", "observe"),
		AccessEnforceLogs:       envBool("DOCTOR_ACCESS_ENFORCE_LOGS", false),
		AccessEnforceTraces:     envBool("DOCTOR_ACCESS_ENFORCE_TRACES", false),
		AccessTeamMapping:       envString("DOCTOR_ACCESS_TEAM_MAPPING", ""),
		AccessGrantsPath:        envString("DOCTOR_ACCESS_GRANTS_PATH", ""),
		AccessOwnersRoot:        envString("DOCTOR_ACCESS_OWNERS_ROOT", "/doctor/v1/endpoints"),
		AccessCacheTTL:          envDuration("DOCTOR_ACCESS_CACHE_TTL", 60*time.Second),
		AccessStaticAllow:       envString("DOCTOR_ACCESS_STATIC_ALLOW", ""),
		AccessStaticAllowGroups: envString("DOCTOR_ACCESS_STATIC_ALLOW_GROUPS", ""),
		AccessStaticDeny:        envString("DOCTOR_ACCESS_STATIC_DENY", ""),
		AccessTrustTenantHeader: envBool("DOCTOR_ACCESS_TRUST_TENANT_HEADER", false),
		AccessPolicyURL:         envString("DOCTOR_ACCESS_POLICY_URL", ""),
		AccessPolicyFile:        envString("DOCTOR_ACCESS_POLICY_FILE", ""),
		AccessPolicyToken:       envString("DOCTOR_ACCESS_POLICY_TOKEN", ""),
		AccessPolicyInterval:    envDuration("DOCTOR_ACCESS_POLICY_INTERVAL", 30*time.Second),
		MonoFSFileToken:         envString("DOCTOR_MONOFS_TOKEN", ""),
		MonoFSClientID:          envString("DOCTOR_MONOFS_CLIENT_ID", "doctor-query"),
		DashboardsStoreRoot:     envString("DOCTOR_DASHBOARDS_STORE_ROOT", "/doctor/v1/dashboards"),
	}
}

// envCSV parses a comma-separated environment variable.
func envCSV(key string, fallback []string) []string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
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
