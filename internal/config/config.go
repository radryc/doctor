package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/quota"
	"github.com/radryc/monofs/pkg/authz/policy"
)

// QuotaConfig is the shared data-plane quota configuration.
type QuotaConfig struct {
	Enabled     bool
	PolicyURL   string
	PolicyToken string
	Interval    time.Duration
	Replicas    int
	Enforce     bool
	Defaults    policy.Quota

	// Durable storage admission state (doctor-ingest).
	StateMode          string
	StateDir           string
	StateReplicaID     string
	StateFlushInterval time.Duration
	StateSharedRoot    string
	StateReserveBytes  int64
	StateFailMode      string
}

// LoadQuota builds the quota configuration for one service. fallbackURL and
// fallbackToken let services reuse DOCTOR_ACCESS_POLICY_* when the dedicated
// quota variables are not set.
func LoadQuota(service, fallbackURL, fallbackToken string) QuotaConfig {
	url := envString("DOCTOR_QUOTA_POLICY_URL", fallbackURL)
	token := envString("DOCTOR_QUOTA_POLICY_TOKEN", fallbackToken)
	enabled := envBool("DOCTOR_QUOTA_ENABLED", strings.TrimSpace(url) != "")
	return QuotaConfig{
		Enabled:     enabled,
		PolicyURL:   url,
		PolicyToken: token,
		Interval:    envDuration("DOCTOR_QUOTA_POLICY_INTERVAL", 30*time.Second),
		Replicas:    int(envInt64("DOCTOR_QUOTA_REPLICAS", 1)),
		Enforce:     envBool("DOCTOR_QUOTA_ENFORCE", false),
		Defaults:    loadQuotaDefaults(),

		StateMode:          envString("DOCTOR_QUOTA_STATE_MODE", "memory"),
		StateDir:           envString("DOCTOR_QUOTA_STATE_DIR", ".doctor/quota"),
		StateReplicaID:     envString("DOCTOR_QUOTA_STATE_REPLICA_ID", defaultReplicaID()),
		StateFlushInterval: envDuration("DOCTOR_QUOTA_STATE_FLUSH_INTERVAL", 10*time.Second),
		StateSharedRoot:    envString("DOCTOR_QUOTA_STATE_SHARED_ROOT", "/doctor/v1/quota/state"),
		StateReserveBytes:  envInt64("DOCTOR_QUOTA_STATE_RESERVE_BYTES", 1<<20),
		StateFailMode:      envString("DOCTOR_QUOTA_STATE_FAIL_MODE", "closed"),
	}
}

func defaultReplicaID() string {
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		return hostname
	}
	return "replica"
}

func loadQuotaDefaults() policy.Quota {
	q := quota.Defaults()
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_QUERY_RPS", 0); value > 0 {
		q.Query.RequestsPerSecond = float64(value)
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_QUERY_CONCURRENT", 0); value > 0 {
		q.Query.MaxConcurrent = int(value)
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_QUERY_BUDGET", 0); value > 0 {
		q.Query.BudgetPerSecond = value
	}
	if value := envDuration("DOCTOR_QUOTA_DEFAULT_QUERY_MAX_RANGE", 0); value > 0 {
		q.Query.MaxRange = value.String()
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_QUERY_MAX_SAMPLES", 0); value > 0 {
		q.Query.MaxSamples = value
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_QUERY_MAX_SERIES", 0); value > 0 {
		q.Query.MaxSeries = value
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_INGEST_BYTES_PER_SECOND", 0); value > 0 {
		q.Ingest.Traces.BytesPerSecond = value
		q.Ingest.Logs.BytesPerSecond = value
		q.Ingest.Metrics.BytesPerSecond = value
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_INGEST_RECORDS_PER_SECOND", 0); value > 0 {
		q.Ingest.Traces.RecordsPerSecond = value
		q.Ingest.Logs.RecordsPerSecond = value
		q.Ingest.Metrics.RecordsPerSecond = value
		q.Ingest.Metrics.SamplesPerSecond = value
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_MAX_SERIES", 0); value > 0 {
		q.Ingest.Metrics.MaxActiveSeries = int(value)
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_RULES_MAX", 0); value > 0 {
		q.Rules.MaxRules = int(value)
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_RULES_COST", 0); value > 0 {
		q.Rules.MaxEvaluationCostPerSecond = value
	}
	if value := envInt64("DOCTOR_QUOTA_DEFAULT_STORAGE_BYTES", 0); value > 0 {
		q.Storage.MaxBytes = value
	}
	return q
}

type PrivacyConfig struct {
	HMACKey string
}

type WALConfig struct {
	Enabled bool
	Dir     string
}

type PipelineConfig struct {
	Enabled         bool
	QueueSize       int
	Workers         int
	DurableRequired bool
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

type IngestConfig struct {
	DefaultTenant       string
	TenantHeader        string
	AuthToken           string
	AuthTokensFile      string
	AllowedTenants      []string
	MemoryLimitBytes    int64
	TLSCertFile         string
	TLSKeyFile          string
	GRPCTLSCertFile     string
	GRPCTLSKeyFile      string
	GRPCMTLSMode        string
	GRPCMTLSCAFile      string
	HTTPMTLSMode        string
	HTTPMTLSCAFile      string
	GRPCAddr            string
	HTTPAddr            string
	DebugEnabled        bool
	DebugAddr           string
	MonoFSLogEngineAddr string
	MonoFSRouterAddr    string
	MonoFSToken         string
	MonoFSClientID      string
	TraceBuffer         TraceBufferConfig
	LogBuffer           LogBufferConfig
	MetricBuffer        MetricBufferConfig
	Privacy             PrivacyConfig
	WAL                 WALConfig
	Pipeline            PipelineConfig
	Quota               QuotaConfig
}

type QueryConfig struct {
	DefaultTenant          string
	TenantHeader           string
	HTTPAddr               string
	TLSCertFile            string
	TLSKeyFile             string
	GuardianURL            string
	GuardianAPIURL         string
	DefaultPartition       string
	MonoFSLogEngineAddr    string
	MonoFSToken            string
	RulesURL               string
	SentinelURL            string
	SentinelServiceToken   string
	SentinelIdentitySecret string
	RulesAPIToken          string
	InternalIdentitySecret string
	AWSEnabled             bool
	AWSBackendURL          string
	AWSBackendToken        string
	GCPEnabled             bool
	GCPBackendURL          string
	GCPBackendToken        string

	FederationEnabled              bool
	FederationCatalogFile          string
	FederationCatalogJSON          string
	FederationSnapshotStore        string
	FederationSnapshotRoot         string
	FederationSnapshotTenant       string
	FederationSnapshotShardTargets int
	FederationTokenResolver        string
	FederationTokenCacheTTL        time.Duration
	FederationTokenBundleRoot      string
	FederationTokenShardCount      int
	FederationTokenKey             string
	FederationReloadInterval       time.Duration
	FederationStaleAfter           time.Duration
	FederationMaxFanout            int
	FederationDiscoveryLimit       int
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

	ProxyEnabled          bool
	ProxyBackendsFile     string
	ProxyRegistryInterval time.Duration
	ProxyMaxInflight      int
	ProxyBackendTimeout   time.Duration
	BackendRequireHTTPS   bool
	BackendAllowLoopback  bool
	BackendAllowPrivate   bool
	BackendAllowedHosts   []string
	// AllowedTenants bounds query tenant names. Empty accepts all names.
	AllowedTenants []string
	// SingleTenant forces every query to one tenant. Empty disables it.
	SingleTenant              string
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

	// Quota enforcement and dashboard usage proxy.
	Quota       QuotaConfig
	IngestURL   string
	IngestToken string
}

// RulesConfig holds doctor-rules settings.
type RulesConfig struct {
	Enabled                bool
	HTTPAddr               string
	TLSCertFile            string
	TLSKeyFile             string
	APIToken               string
	AuthTokensFile         string
	InternalIdentitySecret string
	QueryURL               string
	QueryToken             string
	Workers                int
	EvalTimeout            time.Duration
	MaxCatchUp             time.Duration
	MaxCatchUpTicks        int
	LeaseTTL               time.Duration
	LeaseRenew             time.Duration
	RulerID                string
	StoreRoot              string
	PollInterval           time.Duration
	MonoFSLogEngineAddr    string
	MonoFSToken            string
	MonoFSClientID         string
	Quota                  QuotaConfig
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
		Enabled:                envBool("DOCTOR_RULES_ENABLED", true),
		HTTPAddr:               envString("DOCTOR_RULES_HTTP_ADDR", ":18081"),
		TLSCertFile:            envString("DOCTOR_TLS_CERT_FILE", ""),
		TLSKeyFile:             envString("DOCTOR_TLS_KEY_FILE", ""),
		APIToken:               envString("DOCTOR_RULES_API_TOKEN", ""),
		AuthTokensFile:         envString("DOCTOR_RULES_TOKENS_FILE", ""),
		InternalIdentitySecret: envString("DOCTOR_INTERNAL_IDENTITY_SECRET", ""),
		QueryURL:               envString("DOCTOR_RULES_QUERY_URL", ""),
		QueryToken:             envString("DOCTOR_RULES_QUERY_TOKEN", ""),
		Workers:                int(envInt64("DOCTOR_RULES_WORKERS", 8)),
		EvalTimeout:            envDuration("DOCTOR_RULES_EVAL_TIMEOUT", 30*time.Second),
		MaxCatchUp:             envDuration("DOCTOR_RULES_MAX_CATCHUP", 0),
		MaxCatchUpTicks:        int(envInt64("DOCTOR_RULES_MAX_CATCHUP_TICKS", 60)),
		LeaseTTL:               envDuration("DOCTOR_RULES_LEASE_TTL", 30*time.Second),
		LeaseRenew:             envDuration("DOCTOR_RULES_LEASE_RENEW", 10*time.Second),
		RulerID:                rulerID,
		StoreRoot:              envString("DOCTOR_RULES_STORE_ROOT", "/doctor/v1/rules"),
		PollInterval:           envDuration("DOCTOR_RULES_POLL_INTERVAL", 30*time.Second),
		MonoFSLogEngineAddr:    envString("DOCTOR_MONOFS_LOGENGINE_ADDR", ""),
		MonoFSToken:            envString("DOCTOR_MONOFS_TOKEN", ""),
		MonoFSClientID:         envString("DOCTOR_MONOFS_CLIENT_ID", "doctor-rules"),
		Quota:                  LoadQuota("doctor-rules", envString("DOCTOR_ACCESS_POLICY_URL", ""), envString("DOCTOR_ACCESS_POLICY_TOKEN", "")),
	}
}

// SentinelConfig holds doctor-sentinel settings.
type SentinelConfig struct {
	Enabled              bool
	HTTPAddr             string
	QueryURL             string
	QueryToken           string
	StoreRoot            string
	DefaultTenant        string
	MonoFSLogEngineAddr  string
	MonoFSToken          string
	MonoFSClientID       string
	GuardianHMACSecret   string
	GuardianEnabled      bool
	GuardianURL          string
	GuardianUIURL        string
	GuardianToken        string
	GuardianPollInterval time.Duration
	ServiceToken         string
	IdentityHMACSecret   string
	AuthAllowAll         bool
	DeliveryEnabled      bool
	PollInterval         time.Duration
	EvalShards           int
	ShardIndex           int
	LeaseTTL             time.Duration
	LeaseRenew           time.Duration
	RulerID              string

	IncidentsEnabled     bool
	IncidentTick         time.Duration
	IncidentAgentURL     string
	IncidentAgentToken   string
	IncidentBaseURL      string
	IncidentAgentTimeout time.Duration
	WebhookRequireHTTPS  bool
	WebhookAllowPrivate  bool
	WebhookAllowLoopback bool

	AWSEnabled        bool
	AWSURL            string
	AWSToken          string
	AWSPollInterval   time.Duration
	AWSRegions        []string
	AWSAlarmSelection string

	GCPEnabled      bool
	GCPURL          string
	GCPToken        string
	GCPPollInterval time.Duration
	GCPProject      string
	GCPRegion       string
	GCPPartition    string
	GCPPolicy       string

	// FCB configures the optional fcm-bridge (FCB) notification backend used
	// by receivers of type "fcb". The integration is disabled when BackendURL
	// is empty unless Enabled is set explicitly.
	FCBEnabled       bool
	FCBBackendURL    string
	FCBWebhookSecret string
	// FCBAckSecret authenticates fcm-bridge device ack callbacks. It defaults
	// to FCBWebhookSecret because the bridge reuses DOCTOR_WEBHOOK_SECRET when
	// DOCTOR_ACK_SECRET is not set.
	FCBAckSecret    string
	FCBDefaultTopic string
	FCBTimeout      time.Duration
}

// LoadSentinel loads the doctor-sentinel configuration from the environment.
func LoadSentinel(service string) SentinelConfig {
	rulerID := envString("DOCTOR_SENTINEL_RULER_ID", "")
	if rulerID == "" {
		if hostname, err := os.Hostname(); err == nil {
			rulerID = hostname
		} else {
			rulerID = "doctor-sentinel"
		}
	}
	fcbBackendURL := envString("DOCTOR_SENTINEL_FCB_BACKEND_URL", envString("DOCTOR_FCB_BACKEND_URL", ""))
	fcbWebhookSecret := envString("DOCTOR_SENTINEL_FCB_WEBHOOK_SECRET", envString("DOCTOR_FCB_WEBHOOK_SECRET", ""))
	return SentinelConfig{
		Enabled:              envBool("DOCTOR_SENTINEL_ENABLED", true),
		HTTPAddr:             envString("DOCTOR_SENTINEL_HTTP_ADDR", ":18082"),
		QueryURL:             envString("DOCTOR_SENTINEL_QUERY_URL", ""),
		QueryToken:           envString("DOCTOR_SENTINEL_QUERY_TOKEN", ""),
		StoreRoot:            envString("DOCTOR_SENTINEL_STORE_ROOT", "/doctor/v1/alerts"),
		DefaultTenant:        envString("DOCTOR_SENTINEL_DEFAULT_TENANT", "default"),
		MonoFSLogEngineAddr:  envString("DOCTOR_MONOFS_LOGENGINE_ADDR", ""),
		MonoFSToken:          envString("DOCTOR_MONOFS_TOKEN", ""),
		MonoFSClientID:       envString("DOCTOR_MONOFS_CLIENT_ID", "doctor-sentinel"),
		GuardianHMACSecret:   envString("DOCTOR_SENTINEL_GUARDIAN_HMAC_SECRET", ""),
		GuardianEnabled:      envBool("DOCTOR_SENTINEL_GUARDIAN_ENABLED", false),
		GuardianURL:          envString("DOCTOR_SENTINEL_GUARDIAN_URL", envString("DOCTOR_GUARDIAN_API_URL", "")),
		GuardianUIURL:        envString("DOCTOR_GUARDIAN_UI_URL", envString("DOCTOR_SENTINEL_GUARDIAN_URL", envString("DOCTOR_GUARDIAN_API_URL", ""))),
		GuardianToken:        envString("DOCTOR_SENTINEL_GUARDIAN_TOKEN", envString("DOCTOR_GUARDIAN_TOKEN", "")),
		GuardianPollInterval: envDuration("DOCTOR_SENTINEL_GUARDIAN_POLL_INTERVAL", 30*time.Second),
		ServiceToken:         envString("DOCTOR_SENTINEL_SERVICE_TOKEN", ""),
		IdentityHMACSecret:   envString("DOCTOR_SENTINEL_IDENTITY_SECRET", ""),
		AuthAllowAll:         envBool("DOCTOR_SENTINEL_AUTH_ALLOW_ALL", false),
		DeliveryEnabled:      envBool("DOCTOR_SENTINEL_DELIVERY_ENABLED", true),
		PollInterval:         envDuration("DOCTOR_SENTINEL_POLL_INTERVAL", 30*time.Second),
		EvalShards:           int(envInt64("DOCTOR_SENTINEL_EVAL_SHARDS", 1)),
		ShardIndex:           int(envInt64("DOCTOR_SENTINEL_SHARD_INDEX", 0)),
		LeaseTTL:             envDuration("DOCTOR_SENTINEL_LEASE_TTL", 30*time.Second),
		LeaseRenew:           envDuration("DOCTOR_SENTINEL_LEASE_RENEW", 10*time.Second),
		RulerID:              rulerID,

		IncidentsEnabled:     envBool("DOCTOR_SENTINEL_INCIDENTS_ENABLED", false),
		IncidentTick:         envDuration("DOCTOR_SENTINEL_INCIDENT_TICK", 15*time.Second),
		IncidentAgentURL:     envString("DOCTOR_SENTINEL_AGENT_URL", ""),
		IncidentAgentToken:   envString("DOCTOR_SENTINEL_AGENT_TOKEN", ""),
		IncidentBaseURL:      envString("DOCTOR_SENTINEL_BASE_URL", ""),
		IncidentAgentTimeout: envDuration("DOCTOR_SENTINEL_AGENT_TIMEOUT", 120*time.Second),
		WebhookRequireHTTPS:  envBool("DOCTOR_SENTINEL_WEBHOOK_REQUIRE_HTTPS", false),
		WebhookAllowPrivate:  envBool("DOCTOR_SENTINEL_WEBHOOK_ALLOW_PRIVATE", false),
		WebhookAllowLoopback: envBool("DOCTOR_SENTINEL_WEBHOOK_ALLOW_LOOPBACK", false),

		AWSEnabled:        envBool("DOCTOR_SENTINEL_AWS_ENABLED", false),
		AWSURL:            envString("DOCTOR_SENTINEL_AWS_URL", ""),
		AWSToken:          envString("DOCTOR_SENTINEL_AWS_TOKEN", ""),
		AWSPollInterval:   envDuration("DOCTOR_SENTINEL_AWS_POLL_INTERVAL", 60*time.Second),
		AWSRegions:        envCSV("DOCTOR_SENTINEL_AWS_REGIONS", nil),
		AWSAlarmSelection: envString("DOCTOR_SENTINEL_AWS_ALARM_SELECTION", ""),

		GCPEnabled:      envBool("DOCTOR_SENTINEL_GCP_ENABLED", false),
		GCPURL:          envString("DOCTOR_SENTINEL_GCP_URL", ""),
		GCPToken:        envString("DOCTOR_SENTINEL_GCP_TOKEN", ""),
		GCPPollInterval: envDuration("DOCTOR_SENTINEL_GCP_POLL_INTERVAL", 60*time.Second),
		GCPProject:      envString("DOCTOR_SENTINEL_GCP_PROJECT", ""),
		GCPRegion:       envString("DOCTOR_SENTINEL_GCP_REGION", ""),
		GCPPartition:    envString("DOCTOR_SENTINEL_GCP_PARTITION", ""),
		GCPPolicy:       envString("DOCTOR_SENTINEL_GCP_POLICY", ""),

		FCBEnabled:       envBool("DOCTOR_SENTINEL_FCB_ENABLED", envBool("DOCTOR_FCB_ENABLED", fcbBackendURL != "")),
		FCBBackendURL:    fcbBackendURL,
		FCBWebhookSecret: fcbWebhookSecret,
		FCBAckSecret:     envString("DOCTOR_SENTINEL_FCB_ACK_SECRET", envString("DOCTOR_FCB_ACK_SECRET", fcbWebhookSecret)),
		FCBDefaultTopic:  envString("DOCTOR_SENTINEL_FCB_DEFAULT_TOPIC", envString("DOCTOR_FCB_DEFAULT_TOPIC", "")),
		FCBTimeout:       envDuration("DOCTOR_SENTINEL_FCB_TIMEOUT", envDuration("DOCTOR_FCB_TIMEOUT", 10*time.Second)),
	}
}

func LoadIngest(service string) (IngestConfig, error) {
	cfg := IngestConfig{
		DefaultTenant:       envString("DOCTOR_DEFAULT_TENANT", "default"),
		TenantHeader:        envString("DOCTOR_TENANT_HEADER", "X-Doctor-Tenant"),
		AuthToken:           envString("DOCTOR_INGEST_AUTH_TOKEN", ""),
		AuthTokensFile:      envString("DOCTOR_INGEST_TOKENS_FILE", ""),
		AllowedTenants:      envCSV("DOCTOR_INGEST_ALLOWED_TENANTS", nil),
		MemoryLimitBytes:    envInt64("DOCTOR_MEMORY_LIMIT_BYTES", 858<<20),
		TLSCertFile:         envString("DOCTOR_TLS_CERT_FILE", ""),
		TLSKeyFile:          envString("DOCTOR_TLS_KEY_FILE", ""),
		GRPCTLSCertFile:     envString("DOCTOR_GRPC_TLS_CERT_FILE", ""),
		GRPCTLSKeyFile:      envString("DOCTOR_GRPC_TLS_KEY_FILE", ""),
		GRPCMTLSMode:        envString("DOCTOR_GRPC_MTLS_MODE", envString("DOCTOR_INGEST_MTLS_MODE", "off")),
		GRPCMTLSCAFile:      envString("DOCTOR_GRPC_MTLS_CA_FILE", envString("DOCTOR_INGEST_MTLS_CA_FILE", "")),
		HTTPMTLSMode:        envString("DOCTOR_HTTP_MTLS_MODE", envString("DOCTOR_INGEST_MTLS_MODE", "off")),
		HTTPMTLSCAFile:      envString("DOCTOR_HTTP_MTLS_CA_FILE", envString("DOCTOR_INGEST_MTLS_CA_FILE", "")),
		GRPCAddr:            envString("DOCTOR_GRPC_ADDR", ":4317"),
		HTTPAddr:            envString("DOCTOR_HTTP_ADDR", ":4318"),
		DebugEnabled:        envBool("DOCTOR_DEBUG_ENABLED", false),
		DebugAddr:           envString("DOCTOR_DEBUG_ADDR", "127.0.0.1:6060"),
		MonoFSLogEngineAddr: envString("DOCTOR_MONOFS_LOGENGINE_ADDR", ""),
		MonoFSRouterAddr:    envString("DOCTOR_MONOFS_ROUTER_ADDR", ""),
		MonoFSToken:         envString("DOCTOR_MONOFS_TOKEN", ""),
		MonoFSClientID:      envString("DOCTOR_MONOFS_CLIENT_ID", "doctor-ingest"),
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
			Enabled:         envBool("DOCTOR_PIPELINE_ENABLED", true),
			QueueSize:       int(envInt64("DOCTOR_PIPELINE_QUEUE_SIZE", 1000)),
			Workers:         int(envInt64("DOCTOR_PIPELINE_WORKERS", 4)),
			DurableRequired: envBool("DOCTOR_INGEST_REQUIRE_DURABLE", false),
		},
	}
	if strings.TrimSpace(cfg.Privacy.HMACKey) == "" {
		return IngestConfig{}, errors.New("DOCTOR_PRIVACY_HMAC_KEY is required")
	}
	if strings.TrimSpace(cfg.AuthToken) == "" && strings.TrimSpace(cfg.AuthTokensFile) == "" {
		return IngestConfig{}, errors.New("DOCTOR_INGEST_AUTH_TOKEN or DOCTOR_INGEST_TOKENS_FILE is required")
	}
	// With a shared token, default to the single configured tenant instead of
	// accepting every header value. Per-collector token files carry their own
	// tenant groups, so the deployment-wide allowlist stays optional unless the
	// operator sets DOCTOR_INGEST_ALLOWED_TENANTS explicitly.
	if len(cfg.AllowedTenants) == 0 && strings.TrimSpace(cfg.AuthTokensFile) == "" {
		cfg.AllowedTenants = []string{cfg.DefaultTenant}
	}
	if err := validateMTLS("grpc", cfg.GRPCMTLSMode, cfg.GRPCMTLSCAFile); err != nil {
		return IngestConfig{}, err
	}
	if err := validateMTLS("http", cfg.HTTPMTLSMode, cfg.HTTPMTLSCAFile); err != nil {
		return IngestConfig{}, err
	}
	cfg.Quota = LoadQuota("doctor-ingest", envString("DOCTOR_ACCESS_POLICY_URL", ""), envString("DOCTOR_ACCESS_POLICY_TOKEN", ""))
	return cfg, nil
}

func validateMTLS(name, mode, caFile string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "off":
		return nil
	case "optional", "require":
		if strings.TrimSpace(caFile) == "" {
			return fmt.Errorf("%s mTLS mode %q requires DOCTOR_%s_MTLS_CA_FILE (or DOCTOR_INGEST_MTLS_CA_FILE)", name, mode, strings.ToUpper(name))
		}
		return nil
	default:
		return fmt.Errorf("%s mTLS mode %q is invalid (want off, optional, or require)", name, mode)
	}
}

func LoadQuery(service string) QueryConfig {
	return QueryConfig{
		DefaultTenant:          envString("DOCTOR_DEFAULT_TENANT", "default"),
		TenantHeader:           envString("DOCTOR_TENANT_HEADER", "X-Doctor-Tenant"),
		HTTPAddr:               envString("DOCTOR_QUERY_HTTP_ADDR", envString("DOCTOR_HTTP_ADDR", ":18080")),
		TLSCertFile:            envString("DOCTOR_TLS_CERT_FILE", ""),
		TLSKeyFile:             envString("DOCTOR_TLS_KEY_FILE", ""),
		GuardianURL:            envString("DOCTOR_GUARDIAN_UI_URL", ""),
		GuardianAPIURL:         envString("DOCTOR_GUARDIAN_API_URL", envString("DOCTOR_GUARDIAN_UI_URL", "")),
		DefaultPartition:       envString("DOCTOR_DEFAULT_PARTITION", ""),
		MonoFSLogEngineAddr:    envString("DOCTOR_MONOFS_LOGENGINE_ADDR", ""),
		MonoFSToken:            envString("DOCTOR_MONOFS_TOKEN", ""),
		RulesURL:               envString("DOCTOR_RULES_URL", ""),
		SentinelURL:            envString("DOCTOR_SENTINEL_URL", ""),
		SentinelServiceToken:   envString("DOCTOR_SENTINEL_SERVICE_TOKEN", ""),
		SentinelIdentitySecret: envString("DOCTOR_SENTINEL_IDENTITY_SECRET", ""),
		RulesAPIToken:          envString("DOCTOR_RULES_API_TOKEN", ""),
		InternalIdentitySecret: envString("DOCTOR_INTERNAL_IDENTITY_SECRET", ""),
		AWSEnabled:             envBool("DOCTOR_AWS_ENABLED", true),
		AWSBackendURL:          envString("DOCTOR_AWS_BACKEND_URL", ""),
		AWSBackendToken:        envString("DOCTOR_AWS_BACKEND_TOKEN", ""),
		GCPEnabled:             envBool("DOCTOR_GCP_ENABLED", true),
		GCPBackendURL:          envString("DOCTOR_GCP_BACKEND_URL", ""),
		GCPBackendToken:        envString("DOCTOR_GCP_BACKEND_TOKEN", ""),

		FederationEnabled:              envBool("DOCTOR_FEDERATION_ENABLED", false),
		FederationCatalogFile:          envString("DOCTOR_FEDERATION_CATALOG_FILE", ""),
		FederationCatalogJSON:          envString("DOCTOR_FEDERATION_CATALOG_JSON", ""),
		FederationSnapshotStore:        envString("DOCTOR_FEDERATION_SNAPSHOT_STORE", "file"),
		FederationSnapshotRoot:         envString("DOCTOR_FEDERATION_SNAPSHOT_ROOT", "/doctor/v1/federation"),
		FederationSnapshotTenant:       envString("DOCTOR_FEDERATION_SNAPSHOT_TENANT", "default"),
		FederationSnapshotShardTargets: int(envInt64("DOCTOR_FEDERATION_SNAPSHOT_SHARD_TARGETS", 500)),
		FederationTokenResolver:        envString("DOCTOR_FEDERATION_TOKEN_RESOLVER", "auto"),
		FederationTokenCacheTTL:        envDuration("DOCTOR_FEDERATION_TOKEN_CACHE_TTL", 5*time.Minute),
		FederationTokenBundleRoot:      envString("DOCTOR_FEDERATION_TOKEN_BUNDLE_ROOT", "/doctor/v1/federation/tokens"),
		FederationTokenShardCount:      int(envInt64("DOCTOR_FEDERATION_TOKEN_SHARD_COUNT", 256)),
		FederationTokenKey:             envString("DOCTOR_FEDERATION_TOKEN_KEY", ""),
		FederationReloadInterval:       envDuration("DOCTOR_FEDERATION_RELOAD_INTERVAL", 30*time.Second),
		FederationStaleAfter:           envDuration("DOCTOR_FEDERATION_STALE_AFTER", 0),
		FederationMaxFanout:            int(envInt64("DOCTOR_FEDERATION_MAX_FANOUT", 32)),
		FederationDiscoveryLimit:       int(envInt64("DOCTOR_FEDERATION_DISCOVERY_LIMIT", 10000)),
		FederationMaxInflight:          int(envInt64("DOCTOR_FEDERATION_MAX_INFLIGHT", 64)),
		FederationMaxConcurrentTargets: int(envInt64("DOCTOR_FEDERATION_MAX_CONCURRENT_TARGETS", 8)),
		FederationMaxAttempts:          int(envInt64("DOCTOR_FEDERATION_MAX_ATTEMPTS", 2)),
		FederationClientPoolMax:        int(envInt64("DOCTOR_FEDERATION_CLIENT_POOL_MAX", 10000)),
		FederationClientIdleTTL:        envDuration("DOCTOR_FEDERATION_CLIENT_IDLE_TTL", 15*time.Minute),
		FederationBreakerThreshold:     int(envInt64("DOCTOR_FEDERATION_BREAKER_THRESHOLD", 2)),
		FederationBreakerCooldown:      envDuration("DOCTOR_FEDERATION_BREAKER_COOLDOWN", 15*time.Second),
		FederationMetadataTTL:          envDuration("DOCTOR_FEDERATION_METADATA_TTL", time.Hour),
		FederationTimeout:              envDuration("DOCTOR_FEDERATION_TIMEOUT", 5*time.Second),
		TraceDiscoveryCacheTTL:         envDuration("DOCTOR_TRACE_DISCOVERY_CACHE_TTL", 30*time.Second),
		TraceDiscoveryWindow:           envDuration("DOCTOR_TRACE_DISCOVERY_WINDOW", time.Hour),

		ProxyEnabled:              envBool("DOCTOR_PROXY_ENABLED", true),
		ProxyBackendsFile:         envString("DOCTOR_PROXY_BACKENDS_FILE", ""),
		ProxyRegistryInterval:     envDuration("DOCTOR_PROXY_REGISTRY_INTERVAL", 30*time.Second),
		ProxyMaxInflight:          int(envInt64("DOCTOR_PROXY_MAX_INFLIGHT", 32)),
		ProxyBackendTimeout:       envDuration("DOCTOR_PROXY_BACKEND_TIMEOUT", 15*time.Second),
		BackendRequireHTTPS:       envBool("DOCTOR_BACKEND_REQUIRE_HTTPS", false),
		BackendAllowLoopback:      envBool("DOCTOR_BACKEND_ALLOW_LOOPBACK", false),
		BackendAllowPrivate:       envBool("DOCTOR_BACKEND_ALLOW_PRIVATE", false),
		BackendAllowedHosts:       envCSV("DOCTOR_BACKEND_ALLOWED_HOSTS", nil),
		AllowedTenants:            envCSV("DOCTOR_QUERY_ALLOWED_TENANTS", nil),
		SingleTenant:              envString("DOCTOR_QUERY_SINGLE_TENANT", ""),
		GuardianDiscoveryEnabled:  envBool("DOCTOR_GUARDIAN_DISCOVERY_ENABLED", false),
		GuardianDiscoveryInterval: envDuration("DOCTOR_GUARDIAN_DISCOVERY_INTERVAL", 30*time.Second),
		GuardianToken:             envString("DOCTOR_GUARDIAN_TOKEN", ""),

		// Authentication is required by default. When no verifier is
		// configured the middleware stays a pass-through, so development
		// deployments are unaffected; once OIDC or break-glass is configured,
		// anonymous access must be an explicit opt-out.
		AuthRequire:       envBool("DOCTOR_AUTH_REQUIRE", true),
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
		Quota:                   LoadQuota("doctor-query", envString("DOCTOR_ACCESS_POLICY_URL", ""), envString("DOCTOR_ACCESS_POLICY_TOKEN", "")),
		IngestURL:               envString("DOCTOR_INGEST_URL", ""),
		IngestToken:             envString("DOCTOR_INGEST_AUTH_TOKEN", ""),
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
