package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/access"
	"github.com/radryc/doctor/internal/federation"
	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/segment"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

// LogEngineBackend is the interface used for logs, metrics, and trace queries.
type LogEngineBackend interface {
	QueryLogs(ctx context.Context, tenant, query, service string, from, to time.Time, limit int) ([]segment.LogRecord, error)
	QueryMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time) ([]segment.MetricPointRecord, error)
	// StreamMetrics yields metric points incrementally so metadata-only callers
	// (label and series discovery) never buffer the full result set in memory.
	StreamMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricPointRecord) error) error
	// StreamMetricSeries yields per-series metadata from the storage chunk
	// series index without reading sample data.
	StreamMetricSeries(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricSeriesMeta) error) error
	QueryTraces(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, error)
}

type Service struct {
	defaultTenant    string
	tenantHeader     string
	guardianURL      string
	guardianAPIURL   string
	defaultPartition string
	logEngine        LogEngineBackend
	aws              proxy.AWSClient
	awsViaRouter     bool
	proxyRouter      *proxy.Router
	guardianClient   *http.Client
	rulesProxy       *httputil.ReverseProxy
	sentinelProxy    *httputil.ReverseProxy
	policyGate       *access.PolicyGate
	dashboardStore   DashboardStore

	traceDiscoveryMu       sync.Mutex
	traceDiscoveryCacheTTL time.Duration
	traceDiscoveryWindow   time.Duration
	traceTagValuesCache    map[string]traceTagValuesCacheEntry
	traceTagsCache         map[string]traceTagsCacheEntry
	traceScopesCache       map[string]traceScopesCacheEntry
}

// SetPolicyGate installs the repo-managed access policy. Logs and traces are
// filtered per partition by the caller's identity; metrics are never filtered.
func (s *Service) SetPolicyGate(gate *access.PolicyGate) {
	if gate == nil {
		return
	}
	s.policyGate = gate
	s.logEngine = &policyBackend{inner: s.logEngine, gate: gate}
}

// SetAWSBackend configures the optional AWS (CloudWatch/X-Ray) federation
// backend served by guardian-pusher-aws. The client may be a legacy single
// pusher or a multi-account federation manager.
func (s *Service) SetAWSBackend(client proxy.AWSClient) {
	if client == nil {
		s.aws = nil
		return
	}
	s.aws = client
}

// SetAWSViaRouter records that proxyRouter already owns the AWS backend. It
// prevents native handlers from querying AWS twice (once through the router
// and once through the direct Service.aws fallback).
func (s *Service) SetAWSViaRouter(viaRouter bool) {
	s.awsViaRouter = viaRouter
}

type federationStatusProvider interface {
	Status() map[string]any
}

type federationScopedStatusProvider interface {
	StatusFor(func(string) bool) map[string]any
}

type federationTargetProvider interface {
	TargetsPage(context.Context, string, string, string, string, string, int) federation.TargetPage
}

type federationScopedTargetProvider interface {
	TargetsPageFor(context.Context, federation.TargetPageOptions) federation.TargetPage
}

type federationPlanProvider interface {
	DebugPlan(context.Context, string, federation.Signal, string, string, string) map[string]any
}

// federationPartitionFilter returns nil when the caller can see the whole
// catalog, or a predicate scoped to the caller's partitions when access policy
// enforcement is active.
func (s *Service) federationPartitionFilter(ctx context.Context) func(string) bool {
	if s.policyGate == nil || !s.policyGate.Enforced() {
		return nil
	}
	id := identityFromContext(ctx)
	return func(partition string) bool {
		return s.policyGate.CanSeePartition(id, partition)
	}
}

func (s *Service) handleFederationStatus(w http.ResponseWriter, r *http.Request) {
	visible := s.federationPartitionFilter(r.Context())
	if provider, ok := s.aws.(federationScopedStatusProvider); ok {
		writeJSON(w, http.StatusOK, provider.StatusFor(visible))
		return
	}
	if visible != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	if provider, ok := s.aws.(federationStatusProvider); ok {
		writeJSON(w, http.StatusOK, provider.Status())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

// handleFederationTargets returns the paginated target detail view. Tokens are
// never included; only routing metadata and circuit health are exposed.
// handleFederationPlan exposes the planner decision for admin debugging.
func (s *Service) handleFederationPlan(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.aws.(federationPlanProvider)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	if s.policyGate != nil && s.policyGate.Enforced() && !s.policyGate.IsAdmin(identityFromContext(r.Context())) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "federation plan is admin only"})
		return
	}
	q := r.URL.Query()
	signal := federation.Signal(strings.ToLower(strings.TrimSpace(q.Get("signal"))))
	writeJSON(w, http.StatusOK, provider.DebugPlan(
		r.Context(),
		q.Get("query"),
		signal,
		q.Get("account"),
		q.Get("region"),
		q.Get("partition"),
	))
}

func (s *Service) handleFederationTargets(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	includeMetadata := false
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("include_metadata"))) {
	case "1", "true", "yes":
		includeMetadata = true
	}
	visible := s.federationPartitionFilter(r.Context())
	opts := federation.TargetPageOptions{
		Account:          r.URL.Query().Get("account"),
		Region:           r.URL.Query().Get("region"),
		Partition:        r.URL.Query().Get("partition"),
		Query:            r.URL.Query().Get("q"),
		Cursor:           r.URL.Query().Get("cursor"),
		Limit:            limit,
		VisiblePartition: visible,
		IncludeMetadata:  includeMetadata,
	}
	if provider, ok := s.aws.(federationScopedTargetProvider); ok {
		writeJSON(w, http.StatusOK, provider.TargetsPageFor(r.Context(), opts))
		return
	}
	if visible != nil {
		writeJSON(w, http.StatusOK, federation.TargetPage{Targets: []map[string]any{}})
		return
	}
	if provider, ok := s.aws.(federationTargetProvider); ok {
		writeJSON(w, http.StatusOK, provider.TargetsPage(
			r.Context(),
			opts.Account,
			opts.Region,
			opts.Partition,
			opts.Query,
			opts.Cursor,
			opts.Limit,
		))
		return
	}
	writeJSON(w, http.StatusOK, federation.TargetPage{Targets: []map[string]any{}})
}

// SetTenantHeader sets the HTTP header used to route queries to a tenant when
// no explicit tenant query parameter is supplied.
func (s *Service) SetTenantHeader(header string) {
	if strings.TrimSpace(header) == "" {
		header = "X-Doctor-Tenant"
	}
	s.tenantHeader = header
}

// SetGuardianURL sets the URL of the Guardian web UI shown as a header link.
func (s *Service) SetGuardianURL(u string) { s.guardianURL = u }

// SetGuardianAPIURL sets the Guardian API base URL used for server-side fetches.
func (s *Service) SetGuardianAPIURL(u string) { s.guardianAPIURL = u }

// SetDefaultPartition sets the partition pre-filled in the topology form.
func (s *Service) SetDefaultPartition(p string) { s.defaultPartition = p }

// SetDashboardStore installs the shared dashboard persistence store.
func (s *Service) SetDashboardStore(store DashboardStore) {
	s.dashboardStore = store
}

// SetRulesURL configures the doctor-rules API base URL used by the UI proxy.
func (s *Service) SetRulesURL(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		s.rulesProxy = nil
		return
	}
	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		s.rulesProxy = nil
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "rules service unavailable: " + err.Error()})
	}
	s.rulesProxy = proxy
}

// SetSentinelURL configures the doctor-sentinel API base URL used by the UI
// proxy. Sentinel is a separate process; doctor-query only forwards reads and
// operator actions.
func (s *Service) SetSentinelURL(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		s.sentinelProxy = nil
		return
	}
	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		s.sentinelProxy = nil
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "sentinel service unavailable: " + err.Error()})
	}
	s.sentinelProxy = proxy
}

// guardianTimelineEntry mirrors Guardian's TimelineEntry JSON structure.
type guardianTimelineEntry struct {
	Kind               string            `json:"kind"`
	Timestamp          time.Time         `json:"timestamp"`
	Intent             string            `json:"intent,omitempty"`
	Asset              string            `json:"asset,omitempty"`
	Status             string            `json:"status"`
	DisplayStatus      string            `json:"displayStatus"`
	Title              string            `json:"title"`
	Message            string            `json:"message"`
	DeploymentRevision string            `json:"deploymentRevision,omitempty"`
	TaskID             string            `json:"taskID,omitempty"`
	Details            map[string]string `json:"details,omitempty"`
}

type guardianHistoryResponse struct {
	GeneratedAt time.Time               `json:"generatedAt"`
	Events      []guardianTimelineEntry `json:"events"`
}

// guardianTopologyNode mirrors Guardian's TopologyNode JSON structure.
type guardianTopologyNode struct {
	ID            string            `json:"id"`
	Label         string            `json:"label"`
	Kind          string            `json:"kind"`
	ParentID      string            `json:"parentID,omitempty"`
	Intent        string            `json:"intent,omitempty"`
	Asset         string            `json:"asset,omitempty"`
	AssetType     string            `json:"assetType,omitempty"`
	Status        string            `json:"status"`
	DisplayStatus string            `json:"displayStatus"`
	Health        string            `json:"health"`
	FlowState     string            `json:"flowState,omitempty"`
	FlowSource    string            `json:"flowSource,omitempty"`
	FlowUpdatedAt time.Time         `json:"flowUpdatedAt,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
}

type guardianTopologyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type guardianTopologyData struct {
	Partition string                 `json:"partition"`
	Nodes     []guardianTopologyNode `json:"nodes"`
	Edges     []guardianTopologyEdge `json:"edges"`
}

// guardianPartitionSummary is a minimal view returned by GET /api/partitions.
type guardianPartitionSummary struct {
	Name              string   `json:"name"`
	Status            string   `json:"status"`
	DisplayStatus     string   `json:"displayStatus"`
	Health            string   `json:"health"`
	LastHealth        string   `json:"lastHealth"`
	LastDisplayStatus string   `json:"lastDisplayStatus"`
	Errors            []string `json:"errors,omitempty"`
}

// fetchGuardianHistory calls GET {guardianURL}/api/partitions/{partition}/history.
// Returns nil, nil when guardianURL is unset.
func (s *Service) fetchGuardianHistory(ctx context.Context, partition string) (*guardianHistoryResponse, error) {
	guardianURL := s.guardianAPIBaseURL()
	if guardianURL == "" {
		return nil, nil
	}
	resp, err := s.guardianGET(ctx,
		fmt.Sprintf("/api/m2m/partitions/%s/history", partition),
		fmt.Sprintf("/api/partitions/%s/history", partition),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return &guardianHistoryResponse{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("guardian history HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out guardianHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode guardian history: %w", err)
	}
	return &out, nil
}

// fetchGuardianPartitions returns all known partition names.
func (s *Service) fetchGuardianPartitions(ctx context.Context) ([]string, error) {
	rows, err := s.fetchGuardianPartitionSummaries(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	return names, nil
}

func (s *Service) fetchGuardianPartitionSummaries(ctx context.Context) ([]guardianPartitionSummary, error) {
	guardianURL := s.guardianAPIBaseURL()
	if guardianURL == "" {
		return nil, nil
	}
	resp, err := s.guardianGET(ctx, "/api/m2m/partitions", "/api/partitions")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var rows []guardianPartitionSummary
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, nil
	}
	return rows, nil
}

// fetchGuardianTopology calls GET {guardianURL}/api/partitions/{partition}/topology.
func (s *Service) fetchGuardianTopology(ctx context.Context, partition string) (*guardianTopologyData, error) {
	guardianURL := s.guardianAPIBaseURL()
	if guardianURL == "" {
		return nil, nil
	}
	resp, err := s.guardianGET(ctx,
		fmt.Sprintf("/api/m2m/partitions/%s/topology", partition),
		fmt.Sprintf("/api/partitions/%s/topology", partition),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return &guardianTopologyData{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return &guardianTopologyData{}, nil
	}
	var out guardianTopologyData
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode guardian topology: %w", err)
	}
	return &out, nil
}

// parseWindow converts strings like "1h", "24h", "7d" to a duration.
func parseWindow(s string) time.Duration {
	s = strings.TrimSpace(strings.ToLower(s))
	switch {
	case strings.HasSuffix(s, "d"):
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	case strings.HasSuffix(s, "h"):
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "h"))
		if n > 0 {
			return time.Duration(n) * time.Hour
		}
	case strings.HasSuffix(s, "m"):
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "m"))
		if n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return time.Hour // default 1h
}

func (s *Service) guardianAPIBaseURL() string {
	if strings.TrimSpace(s.guardianAPIURL) != "" {
		return s.guardianAPIURL
	}
	return s.guardianURL
}

func (s *Service) guardianGET(ctx context.Context, m2mPath, legacyPath string) (*http.Response, error) {
	base := strings.TrimRight(s.guardianAPIBaseURL(), "/")
	// Prefer the user-facing API: it enforces the caller's repo ACL and
	// returns the same data the Guardian UI shows. The M2M aliases are a
	// fallback for service-to-service callers.
	paths := []string{legacyPath, m2mPath}
	for i, p := range paths {
		url := base + p
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if token := guardianTokenFromContext(ctx); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := s.guardianClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound && i == 0 {
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("guardian request failed for %s", m2mPath)
}

func guardianHealthFromPartitionSummaries(partition string, summaries []guardianPartitionSummary) map[string]any {
	if len(summaries) == 0 {
		return nil
	}

	if partition != "" {
		for _, summary := range summaries {
			if summary.Name != partition {
				continue
			}
			level, score, recommendation, reason := guardianPartitionHealthPresentation(summary)
			return map[string]any{
				"partition": partition,
				"overall_health": map[string]any{
					"score":    score,
					"level":    level,
					"reason":   reason,
					"check_at": time.Now().UTC(),
				},
				"recent_failures": 0,
				"recent_drifts":   0,
				"recommendation":  recommendation,
			}
		}
		return nil
	}

	var healthy, attention, pending, failing int
	for _, summary := range summaries {
		switch guardianSummaryLevel(summary) {
		case "healthy":
			healthy++
		case "attention":
			attention++
		case "pending":
			pending++
		case "failing":
			failing++
		}
	}

	total := healthy + attention + pending + failing
	if total == 0 {
		return nil
	}

	level := "healthy"
	score := 1.0
	recommendation := "proceed"
	reason := fmt.Sprintf("%d partition(s) stable", healthy)
	switch {
	case failing > 0:
		level = "failing"
		score = 0.2
		recommendation = "rollback"
		reason = fmt.Sprintf("%d partition(s) need action", failing)
	case attention > 0:
		level = "attention"
		score = 0.6
		recommendation = "hold"
		reason = fmt.Sprintf("%d partition(s) need attention", attention)
	case pending > 0:
		level = "pending"
		score = 0.75
		recommendation = "hold"
		reason = fmt.Sprintf("%d partition(s) progressing", pending)
	}

	return map[string]any{
		"partition": partition,
		"overall_health": map[string]any{
			"score":    score,
			"level":    level,
			"reason":   reason,
			"check_at": time.Now().UTC(),
		},
		"recent_failures": 0,
		"recent_drifts":   0,
		"recommendation":  recommendation,
	}
}

func guardianPartitionHealthPresentation(summary guardianPartitionSummary) (level string, score float64, recommendation, reason string) {
	level = guardianSummaryLevel(summary)
	display := strings.TrimSpace(summary.DisplayStatus)
	if display == "" {
		display = strings.TrimSpace(summary.LastDisplayStatus)
	}
	if display == "" {
		display = strings.TrimSpace(summary.Status)
	}
	if len(summary.Errors) > 0 && strings.TrimSpace(summary.Errors[0]) != "" {
		reason = strings.TrimSpace(summary.Errors[0])
	} else {
		reason = display
	}

	switch level {
	case "healthy":
		return level, 1.0, "proceed", valueOrDefault(reason, "Stable")
	case "attention":
		return level, 0.6, "hold", valueOrDefault(reason, "Attention")
	case "pending":
		return level, 0.75, "hold", valueOrDefault(reason, "Progressing")
	case "failing":
		return level, 0.2, "rollback", valueOrDefault(reason, "Needs action")
	default:
		return "unknown", 0.5, "", valueOrDefault(reason, "Guardian status unknown")
	}
}

func guardianSummaryLevel(summary guardianPartitionSummary) string {
	level := strings.TrimSpace(summary.Health)
	if level == "" {
		level = strings.TrimSpace(summary.LastHealth)
	}
	return level
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// NewServiceLogEngine creates a query service backed by the monofs logengine.
func NewServiceLogEngine(c *monofsstore.LogEngineClient, defaultTenant string) *Service {
	return &Service{
		defaultTenant: defaultTenant,
		logEngine:     c,
		guardianClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/ready", s.handleLokiReady)
	mux.HandleFunc("/api/v1/query", s.handlePrometheusQuery)
	mux.HandleFunc("/api/v1/query_range", s.handlePrometheusQueryRange)
	mux.HandleFunc("/api/v1/labels", s.handlePrometheusLabels)
	mux.HandleFunc("/api/v1/label/", s.handlePrometheusLabelValues)
	mux.HandleFunc("/api/v1/series", s.handlePrometheusSeries)
	mux.HandleFunc("/api/v1/metadata", s.handlePrometheusMetadata)
	mux.HandleFunc("/api/v1/query_exemplars", s.handlePrometheusQueryExemplars)
	mux.HandleFunc("/api/v1/rules", s.handlePrometheusRules)
	mux.HandleFunc("/api/v1/status/buildinfo", s.handlePrometheusBuildInfo)
	mux.HandleFunc("/loki/api/v1/query", s.handleLokiQuery)
	mux.HandleFunc("/loki/api/v1/query_range", s.handleLokiQueryRange)
	mux.HandleFunc("/loki/api/v1/labels", s.handleLokiLabels)
	mux.HandleFunc("/loki/api/v1/label/", s.handleLokiLabelValues)
	mux.HandleFunc("/loki/api/v1/series", s.handleLokiSeries)
	mux.HandleFunc("/loki/api/v1/index/stats", s.handleLokiIndexStats)
	mux.HandleFunc("/loki/api/v1/index/volume", s.handleLokiIndexVolume)
	mux.HandleFunc("/loki/api/v1/index/volume_range", s.handleLokiIndexVolumeRange)
	mux.HandleFunc("/loki/api/v1/patterns", s.handleLokiPatterns)
	mux.HandleFunc("/loki/api/v1/detected_fields", s.handleLokiDetectedFields)
	mux.HandleFunc("/loki/api/v1/detected_labels", s.handleLokiDetectedLabels)
	mux.HandleFunc("/loki/api/v1/status/buildinfo", s.handleLokiBuildInfo)
	mux.HandleFunc("/api/echo", s.handleTempoEcho)
	mux.HandleFunc("/api/status/buildinfo", s.handleTempoBuildInfo)
	mux.HandleFunc("/api/traces/", s.handleTempoTraceByID)
	mux.HandleFunc("/api/v2/traces/", s.handleTempoTraceByIDV2)
	mux.HandleFunc("/api/search", s.handleTempoSearch)
	mux.HandleFunc("/api/search/tags", s.handleTempoTags)
	mux.HandleFunc("/api/search/tag/", s.handleTempoTagValues)
	mux.HandleFunc("/api/v2/search/tags", s.handleTempoTagsV2)
	mux.HandleFunc("/api/v2/search/tag/", s.handleTempoTagValuesV2)
	mux.HandleFunc("/api/metrics/query_range", s.handleTempoMetricsQueryRange)
	mux.HandleFunc("/api/metrics/query", s.handleTempoMetricsQuery)
	mux.HandleFunc("/v1/spans/search", s.handleSpanSearch)
	mux.HandleFunc("/v1/traces/recent", s.handleRecentTraces)
	mux.HandleFunc("/v1/traces/", s.handleTraceByID)
	mux.HandleFunc("/v1/logs/services", s.handleLogServices)
	mux.HandleFunc("/v1/logs/search", s.handleLogSearch)
	mux.HandleFunc("/v1/federation/status", s.handleFederationStatus)
	mux.HandleFunc("/v1/federation/targets", s.handleFederationTargets)
	mux.HandleFunc("/v1/federation/plan", s.handleFederationPlan)
	mux.HandleFunc("/v1/metrics/range", s.handleMetricRange)
	mux.HandleFunc("/v1/rules", s.handleRulesProxy)
	mux.HandleFunc("/v1/rules/", s.handleRulesProxy)
	mux.HandleFunc("/v1/alerts", s.handleSentinelProxy)
	mux.HandleFunc("/v1/alerts/", s.handleSentinelProxy)
	mux.HandleFunc("/v1/services/discover", s.handleServiceDiscovery)
	mux.HandleFunc("/v1/services/suggest", s.handleServiceSuggest)
	mux.HandleFunc("/v1/dashboards", s.handleDashboardsRouter)
	mux.HandleFunc("/v1/dashboards/", s.handleDashboardsRouter)
	mux.HandleFunc("/v1/proxy/endpoints", s.handleProxyEndpoints)
	mux.HandleFunc("/v1/guardian/topology", s.handleGuardianTopology)
	mux.HandleFunc("/v1/guardian/events", s.handleGuardianEvents)
	mux.HandleFunc("/v1/guardian/health", s.handleGuardianHealth)
	mux.HandleFunc("/v1/guardian/overview", s.handleGuardianOverview)
	mux.HandleFunc("/v1/guardian/probes", s.handleGuardianProbes)
	mux.HandleFunc("/v1/ui/config", s.handleUIConfig)
	mux.HandleFunc("/v1/ui/user", s.handleUIUser)
	mux.HandleFunc("/", s.handleUI)
	return withWarningHeaders(s.guardianTokenMiddleware(mux))
}

// guardianTokenKey carries the caller's OIDC token to server-side Guardian
// requests. Guardian enforces its repo ACL on /api/partitions (and the M2M
// aliases), so the Doctor must forward the user's identity instead of calling
// anonymously and receiving an empty overview.
type guardianTokenKey struct{}

func withGuardianToken(ctx context.Context, token string) context.Context {
	if strings.TrimSpace(token) == "" {
		return ctx
	}
	return context.WithValue(ctx, guardianTokenKey{}, strings.TrimSpace(token))
}

func guardianTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(guardianTokenKey{}).(string)
	return token
}

func guardianTokenFromRequest(r *http.Request) string {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	if cookie, err := r.Cookie("doctor_session"); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}

func (s *Service) guardianTokenMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := guardianTokenFromRequest(r); token != "" {
			r = r.WithContext(withGuardianToken(r.Context(), token))
		}
		next.ServeHTTP(w, r)
	})
}

// handleRulesProxy exposes the doctor-rules list/detail and manual-evaluate
// endpoints through the query UI origin. Mutations stay on the rules service
// API; the UI is read-only except for manual evaluation.
func (s *Service) handleRulesProxy(w http.ResponseWriter, r *http.Request) {
	if s.rulesProxy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "recording rules service is not configured"})
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		if r.URL.Path != "/v1/rules/evaluate" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.rulesProxy.ServeHTTP(w, r)
}

// handleSentinelProxy forwards alert reads and operator actions to
// doctor-sentinel. Identity headers are derived from the caller's verified
// identity; Sentinel performs the authoritative capability checks.
func (s *Service) handleSentinelProxy(w http.ResponseWriter, r *http.Request) {
	if s.sentinelProxy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sentinel service is not configured"})
		return
	}
	cloned := r.Clone(r.Context())
	if identity, ok := authz.IdentityFromContext(r.Context()); ok {
		display := identity.Email
		if display == "" {
			display = identity.Subject
		}
		if display == "" {
			display = identity.ClientID
		}
		if display != "" {
			cloned.Header.Set("X-Doctor-User", display)
		}
		if len(identity.Groups) > 0 {
			cloned.Header.Set("X-Doctor-Groups", strings.Join(identity.Groups, ","))
		}
		if s.policyGate != nil && s.policyGate.IsAdmin(identity) {
			cloned.Header.Set("X-Doctor-Admin", "true")
			cloned.Header.Set("X-Doctor-Partition-Access", "*")
		}
	}
	s.sentinelProxy.ServeHTTP(w, cloned)
}

func (s *Service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleUIConfig(w http.ResponseWriter, _ *http.Request) {
	defaultTenant := strings.TrimSpace(s.defaultTenant)
	if defaultTenant == "" {
		defaultTenant = "default"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"default_tenant":    defaultTenant,
		"guardian_url":      s.guardianURL,
		"default_partition": s.defaultPartition,
	})
}

// handleUIUser reports the signed-in caller for the header.
func (s *Service) handleUIUser(w http.ResponseWriter, r *http.Request) {
	identity, _ := authz.IdentityFromContext(r.Context())
	display := identity.Email
	if display == "" {
		display = identity.Subject
	}
	if display == "" {
		display = identity.ClientID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": !identity.IsAnonymous(),
		"subject":       identity.Subject,
		"email":         identity.Email,
		"groups":        identity.Groups,
		"client_id":     identity.ClientID,
		"display":       display,
	})
}

func (s *Service) handleTraceByID(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimPrefix(r.URL.Path, "/v1/traces/")
	if traceID == "" {
		http.Error(w, "trace id is required", http.StatusBadRequest)
		return
	}
	tenant := s.tenantFromRequest(r)
	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC()
	traceID, err := s.resolveTraceID(r.Context(), tenant, traceID, from, to)
	if err != nil {
		if isAccessDenied(err) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if traceID == "" {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}

	var spans []segment.TraceRecord
	if s.proxyRouter != nil {
		records, warnings, err := s.proxyRouter.GetTrace(r.Context(), tenant, traceID)
		if err != nil {
			if isAccessDenied(err) {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(records) == 0 {
			http.Error(w, "trace not found", http.StatusNotFound)
			return
		}
		writeProxyWarnings(w, warnings)
		spans = records
	} else {
		var err error
		spans, err = s.logEngine.QueryTraces(r.Context(), tenant, traceID, "", from, to, 1000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(spans) == 0 {
			http.Error(w, "trace not found", http.StatusNotFound)
			return
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].StartTime.Before(spans[j].StartTime) })
	writeJSON(w, http.StatusOK, map[string]any{
		"trace_id": traceID,
		"spans":    spans,
	})
}

func (s *Service) handleSpanSearch(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	traceID := strings.TrimSpace(r.URL.Query().Get("trace_id"))
	from := parseTime(r.URL.Query().Get("from"))
	to := parseTime(r.URL.Query().Get("to"))
	limit := parseLimit(r.URL.Query().Get("limit"), 100, 1000)

	if from.IsZero() {
		from = time.Now().UTC().Add(-1 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}

	var spans []segment.TraceRecord
	if s.proxyRouter != nil {
		var err error
		spans, err = s.searchSpansViaRouter(r, tenant, service, traceID, from, to, limit)
		if err != nil {
			if isAccessDenied(err) {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		var err error
		spans, err = s.logEngine.QueryTraces(r.Context(), tenant, traceID, service, from, to, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].StartTime.Equal(spans[j].StartTime) {
			return spans[i].SpanID < spans[j].SpanID
		}
		return spans[i].StartTime.After(spans[j].StartTime)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  service,
		"trace_id": traceID,
		"returned": len(spans),
		"spans":    spans,
	})
}

func (s *Service) handleLogSearch(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	identity := identityFromRequest(r)
	partition := identity.Partition
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	logGroup := strings.TrimSpace(r.URL.Query().Get("logGroup"))
	if logGroup == "" {
		logGroup = strings.TrimSpace(r.URL.Query().Get("log_group"))
	}
	from := parseTime(r.URL.Query().Get("from"))
	to := parseTime(r.URL.Query().Get("to"))
	limit := parseLimit(r.URL.Query().Get("limit"), 100, 1000)

	if from.IsZero() {
		from = time.Now().UTC().Add(-1 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}

	// Reject broad live-AWS searches before any fan-out. Native/local queries
	// are unaffected; this only applies when the query or log-group picker
	// targets AWS.
	if _, awsMode := proxy.AWSQueryMode(query); awsMode != proxy.AWSQueryNone || logGroup != "" {
		groups := []string(nil)
		if logGroup != "" {
			groups = []string{logGroup}
		}
		if err := s.checkAWSLogScope(r.Context(), query, groups); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
	}

	var results []segment.LogRecord
	if s.proxyRouter != nil {
		records, warnings, err := s.proxyRouter.Logs(r.Context(), tenant, logSearchPushdown(service, query, logGroup, identity), from, to, limit)
		if err != nil {
			if isAccessDenied(err) {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// The proxy router serves native storage and, when configured, the
		// single logical AWS federated backend. Only use the direct AWS
		// fallback when the router does not own AWS; otherwise the same query
		// would be sent to AWS twice.
		if !s.awsViaRouter {
			// When the search explicitly selects the AWS source, also query
			// CloudWatch and merge the records. The CloudWatch log-group picker
			// is an AWS selector too, so it must not depend on the free-text
			// query containing source="aws".
			if logGroup != "" {
				plan := lokiPlan{selector: fmt.Sprintf(`{source="aws", aws_log_group=%q}`, logGroup)}
				if awsLogs := s.queryAWSLogsForLoki(r.Context(), tenant, plan, from, to, limit); len(awsLogs) > 0 {
					records = proxy.MergeLogs(records, proxy.LogRemote{Source: proxy.SourceAWS, Records: awsLogs})
				}
			} else if plan, parseErr := parseLokiPlan(query); parseErr == nil {
				if awsLogs := s.queryAWSLogsForLoki(r.Context(), tenant, plan, from, to, limit); len(awsLogs) > 0 {
					records = proxy.MergeLogs(records, proxy.LogRemote{Source: proxy.SourceAWS, Records: awsLogs})
				}
			}
		}
		writeProxyWarnings(w, warnings)
		results = records
	} else if logGroup != "" {
		// A log group selector is AWS-specific; without the proxy router there
		// is no CloudWatch backend to query.
		results = nil
	} else {
		var err error
		if raw, ok := s.logEngine.(rawLogQueryBackend); ok {
			selector := identity.logSelector(service)
			if query != "" {
				selector += " |= " + strconv.Quote(query)
			}
			results, err = raw.QueryLogsRaw(r.Context(), tenant, selector, from, to, limit)
		} else {
			results, err = s.logEngine.QueryLogs(r.Context(), tenant, query, service, from, to, limit)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if service != "" || identity.active() {
		filtered := results[:0]
		for _, result := range results {
			if service != "" && result.Service != service {
				continue
			}
			resultPartition, resultIntent, resultAsset := logIdentity(result)
			if !identity.matches(resultPartition, resultIntent, resultAsset) {
				continue
			}
			filtered = append(filtered, result)
		}
		results = filtered
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Timestamp.After(results[j].Timestamp) })

	writeJSON(w, http.StatusOK, map[string]any{
		"service":   service,
		"query":     query,
		"partition": partition,
		"intent":    identity.Intent,
		"asset":     identity.Asset,
		"returned":  len(results),
		"results":   results,
	})
}

func (s *Service) handleMetricRange(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	metricName := strings.TrimSpace(r.URL.Query().Get("metric"))
	if metricName == "" {
		http.Error(w, "metric is required", http.StatusBadRequest)
		return
	}
	from := parseTime(r.URL.Query().Get("from"))
	to := parseTime(r.URL.Query().Get("to"))
	if from.IsZero() {
		from = time.Now().UTC().Add(-1 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	points := make([]segment.MetricPointRecord, 0, 1024)
	scanned := 0
	err := s.logEngine.StreamMetrics(r.Context(), tenant, segment.MetricQuery{MetricName: metricName}, from, to, func(point segment.MetricPointRecord) error {
		scanned++
		if scanned > maxMetricRangePoints {
			return errMetricQueryTooBroad
		}
		points = append(points, point)
		return nil
	})
	if err != nil {
		if errors.Is(err, errMetricQueryTooBroad) {
			http.Error(w, "metric has too many points; narrow the time range", http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	writeJSON(w, http.StatusOK, map[string]any{
		"metric": metricName,
		"points": points,
	})
}

func (s *Service) handleGuardianTopology(w http.ResponseWriter, r *http.Request) {
	partition := strings.TrimSpace(r.URL.Query().Get("partition"))
	tenant := s.tenantFromRequest(r)

	type outNode struct {
		ID            string            `json:"id"`
		Type          string            `json:"type"`
		Name          string            `json:"name"`
		Partition     string            `json:"partition"`
		ParentID      string            `json:"parent_id,omitempty"`
		Intent        string            `json:"intent,omitempty"`
		Asset         string            `json:"asset,omitempty"`
		Status        string            `json:"status"`
		TargetPusher  string            `json:"target_pusher,omitempty"`
		LastEventAt   string            `json:"last_event_at"`
		FlowState     string            `json:"flow_state,omitempty"`
		FlowSource    string            `json:"flow_source,omitempty"`
		FlowUpdatedAt string            `json:"flow_updated_at,omitempty"`
		Health        map[string]any    `json:"health"`
		Meta          map[string]string `json:"meta,omitempty"`
	}
	type outEdge struct {
		From     string `json:"from"`
		To       string `json:"to"`
		Relation string `json:"relation"`
	}

	if s.guardianAPIBaseURL() == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant":    tenant,
			"partition": partition,
			"nodes":     []any{},
			"edges":     []any{},
			"built_at":  time.Now().UTC(),
		})
		return
	}

	// Fleet-wide: fetch topology for all partitions and merge.
	if partition == "" {
		allNodes, allEdges := s.fetchAllGuardianTopologies(r.Context())
		out := make([]outNode, 0, len(allNodes))
		for _, n := range allNodes {
			healthScore, healthLevel := doctorNodeHealthFromGuardian(n.guardianTopologyNode)
			meta := buildMetaFromGuardianNode(n)
			out = append(out, outNode{
				ID:            n.ID,
				Type:          n.Kind,
				Name:          n.Label,
				Partition:     n.Partition,
				ParentID:      n.ParentID,
				Intent:        n.Intent,
				Asset:         n.Asset,
				Status:        n.Status,
				FlowState:     strings.TrimSpace(n.FlowState),
				FlowSource:    strings.TrimSpace(n.FlowSource),
				FlowUpdatedAt: formatRFC3339OrEmpty(n.FlowUpdatedAt),
				Health:        map[string]any{"score": healthScore, "level": healthLevel},
				Meta:          meta,
			})
		}
		edges := make([]outEdge, 0, len(allEdges))
		for _, e := range allEdges {
			edges = append(edges, outEdge{From: e.From, To: e.To, Relation: e.Kind})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant":   tenant,
			"nodes":    out,
			"edges":    edges,
			"built_at": time.Now().UTC(),
		})
		return
	}

	// Single partition topology.
	topo, err := s.fetchGuardianTopology(r.Context(), partition)
	if err != nil || topo == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant":    tenant,
			"partition": partition,
			"nodes":     []any{},
			"edges":     []any{},
			"built_at":  time.Now().UTC(),
		})
		return
	}

	nodes := make([]outNode, 0, len(topo.Nodes))
	for _, n := range topo.Nodes {
		healthScore, healthLevel := doctorNodeHealthFromGuardian(n)
		meta := buildMetaFromGuardianNode(n)
		nodes = append(nodes, outNode{
			ID:            n.ID,
			Type:          n.Kind,
			Name:          n.Label,
			Partition:     partition,
			ParentID:      n.ParentID,
			Intent:        n.Intent,
			Asset:         n.Asset,
			Status:        n.Status,
			FlowState:     strings.TrimSpace(n.FlowState),
			FlowSource:    strings.TrimSpace(n.FlowSource),
			FlowUpdatedAt: formatRFC3339OrEmpty(n.FlowUpdatedAt),
			Health:        map[string]any{"score": healthScore, "level": healthLevel},
			Meta:          meta,
		})
	}
	edges := make([]outEdge, 0, len(topo.Edges))
	for _, e := range topo.Edges {
		edges = append(edges, outEdge{From: e.From, To: e.To, Relation: e.Kind})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":    tenant,
		"partition": partition,
		"nodes":     nodes,
		"edges":     edges,
		"built_at":  time.Now().UTC(),
	})
}

type augmentedTopologyNode struct {
	guardianTopologyNode
	Partition string
}

// fetchAllGuardianTopologies fetches topology for every partition and merges them.
func (s *Service) fetchAllGuardianTopologies(ctx context.Context) ([]augmentedTopologyNode, []guardianTopologyEdge) {
	partitions, err := s.fetchGuardianPartitions(ctx)
	if err != nil || len(partitions) == 0 {
		return nil, nil
	}

	type result struct {
		nodes []augmentedTopologyNode
		edges []guardianTopologyEdge
	}
	ch := make(chan result, len(partitions))

	for _, p := range partitions {
		go func(part string) {
			topo, err := s.fetchGuardianTopology(ctx, part)
			if err != nil || topo == nil {
				ch <- result{}
				return
			}
			nodes := make([]augmentedTopologyNode, 0, len(topo.Nodes))
			for _, n := range topo.Nodes {
				nodes = append(nodes, augmentedTopologyNode{
					guardianTopologyNode: n,
					Partition:            part,
				})
			}
			ch <- result{nodes: nodes, edges: topo.Edges}
		}(p)
	}

	var allNodes []augmentedTopologyNode
	var allEdges []guardianTopologyEdge
	for range partitions {
		r := <-ch
		allNodes = append(allNodes, r.nodes...)
		allEdges = append(allEdges, r.edges...)
	}
	return allNodes, allEdges
}

func buildMetaFromGuardianNode(n any) map[string]string {
	meta := map[string]string{}
	switch node := n.(type) {
	case augmentedTopologyNode:
		for k, v := range node.Meta {
			meta[k] = v
		}
		if strings.TrimSpace(node.FlowState) != "" {
			meta["flow_state"] = node.FlowState
		}
		if strings.TrimSpace(node.FlowSource) != "" {
			meta["flow_source"] = node.FlowSource
		}
		if !node.FlowUpdatedAt.IsZero() {
			meta["flow_updated_at"] = node.FlowUpdatedAt.UTC().Format(time.RFC3339)
		}
	case guardianTopologyNode:
		for k, v := range node.Meta {
			meta[k] = v
		}
		if strings.TrimSpace(node.FlowState) != "" {
			meta["flow_state"] = node.FlowState
		}
		if strings.TrimSpace(node.FlowSource) != "" {
			meta["flow_source"] = node.FlowSource
		}
		if !node.FlowUpdatedAt.IsZero() {
			meta["flow_updated_at"] = node.FlowUpdatedAt.UTC().Format(time.RFC3339)
		}
	}
	return meta
}

func doctorNodeHealthFromGuardian(node guardianTopologyNode) (float64, string) {
	flow := strings.ToLower(strings.TrimSpace(node.FlowState))
	switch flow {
	case "healthy":
		return 1.0, "healthy"
	case "busy":
		return 0.6, "degraded"
	case "failing":
		return 0.2, "unhealthy"
	case "unknown":
		return 0.5, "unknown"
	}
	healthLevel := strings.TrimSpace(node.Health)
	if healthLevel == "" {
		healthLevel = "unknown"
	}
	return 0.5, healthLevel
}

func formatRFC3339OrEmpty(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

func (s *Service) handleGuardianEvents(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	partition := strings.TrimSpace(r.URL.Query().Get("partition"))
	intent := strings.TrimSpace(r.URL.Query().Get("intent"))
	window := parseWindow(r.URL.Query().Get("window"))
	limitStr := r.URL.Query().Get("limit")
	limit := parseLimit(limitStr, 200, 1000)

	cutoff := time.Now().UTC().Add(-window)

	if s.guardianAPIBaseURL() == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant":    tenant,
			"partition": partition,
			"intent":    intent,
			"returned":  0,
			"events":    []any{},
		})
		return
	}

	// Collect partitions to query.
	partitions := []string{partition}
	if partition == "" {
		var err error
		partitions, err = s.fetchGuardianPartitions(r.Context())
		if err != nil || len(partitions) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"tenant": tenant, "partition": partition, "intent": intent,
				"returned": 0, "events": []any{},
			})
			return
		}
	}

	type outEvent struct {
		Tenant    string `json:"tenant"`
		Partition string `json:"partition"`
		Intent    string `json:"intent,omitempty"`
		Asset     string `json:"asset,omitempty"`
		Kind      string `json:"kind"`
		Timestamp string `json:"timestamp"`
		Status    string `json:"status"`
		Message   string `json:"message,omitempty"`
		TaskID    string `json:"task_id,omitempty"`
		Revision  string `json:"revision,omitempty"`
	}

	var all []outEvent
	for _, p := range partitions {
		hist, err := s.fetchGuardianHistory(r.Context(), p)
		if err != nil || hist == nil {
			continue
		}
		for _, e := range hist.Events {
			if e.Timestamp.Before(cutoff) {
				continue
			}
			if intent != "" && e.Intent != "" && e.Intent != intent {
				continue
			}
			all = append(all, outEvent{
				Tenant:    tenant,
				Partition: p,
				Intent:    e.Intent,
				Asset:     e.Asset,
				Kind:      e.Kind,
				Timestamp: e.Timestamp.UTC().Format(time.RFC3339),
				Status:    e.Status,
				Message:   e.Message,
				TaskID:    e.TaskID,
				Revision:  e.DeploymentRevision,
			})
		}
	}

	// Sort newest first.
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp > all[j].Timestamp })
	if len(all) > limit {
		all = all[:limit]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":    tenant,
		"partition": partition,
		"intent":    intent,
		"returned":  len(all),
		"events":    all,
	})
}

func (s *Service) handleGuardianHealth(w http.ResponseWriter, r *http.Request) {
	partition := strings.TrimSpace(r.URL.Query().Get("partition"))
	intent := strings.TrimSpace(r.URL.Query().Get("intent"))
	windowStr := strings.TrimSpace(r.URL.Query().Get("window"))
	if windowStr == "" {
		windowStr = "1h"
	}
	window := parseWindow(windowStr)
	cutoff := time.Now().UTC().Add(-window)

	unknown := map[string]any{
		"partition": partition,
		"intent":    intent,
		"overall_health": map[string]any{
			"score":  0.5,
			"level":  "unknown",
			"reason": "no guardian url configured",
		},
		"recent_failures": 0,
		"recent_drifts":   0,
		"recommendation":  "",
		"window":          windowStr,
		"computed_at":     time.Now().UTC(),
	}

	if s.guardianAPIBaseURL() == "" {
		writeJSON(w, http.StatusOK, unknown)
		return
	}

	// Collect partitions to evaluate.
	partitions := []string{partition}
	if partition == "" {
		var err error
		partitions, err = s.fetchGuardianPartitions(r.Context())
		if err != nil || len(partitions) == 0 {
			unknown["overall_health"] = map[string]any{
				"score": 0.5, "level": "unknown",
				"reason": "could not list guardian partitions",
			}
			writeJSON(w, http.StatusOK, unknown)
			return
		}
	}

	var failures, drifts, successes int
	var latestEvent time.Time

	for _, p := range partitions {
		hist, err := s.fetchGuardianHistory(r.Context(), p)
		if err != nil || hist == nil {
			continue
		}
		for _, e := range hist.Events {
			if e.Timestamp.Before(cutoff) {
				continue
			}
			if intent != "" && e.Intent != "" && e.Intent != intent {
				continue
			}
			if e.Timestamp.After(latestEvent) {
				latestEvent = e.Timestamp
			}
			switch {
			case e.Status == "failed" || e.Status == "error" ||
				e.Details["status"] == "Failed" || e.Details["status"] == "Error":
				failures++
			case e.Details["status"] == "Blocked":
				drifts++
			case e.Status == "healthy":
				successes++
			}
		}
	}

	total := successes + failures
	if total == 0 && drifts == 0 {
		if intent == "" {
			summaries, err := s.fetchGuardianPartitionSummaries(r.Context())
			if err == nil {
				if live := guardianHealthFromPartitionSummaries(partition, summaries); live != nil {
					live["intent"] = intent
					live["window"] = windowStr
					live["computed_at"] = time.Now().UTC()
					writeJSON(w, http.StatusOK, live)
					return
				}
			}
		}

		// No signal in window.
		unknown["overall_health"] = map[string]any{
			"score":  0.5,
			"level":  "unknown",
			"reason": "no events in window",
		}
		unknown["window"] = windowStr
		writeJSON(w, http.StatusOK, unknown)
		return
	}

	var score float64
	if total > 0 {
		score = float64(successes) / float64(total)
	} else {
		score = 1.0 // only drifts, no hard failures
	}
	if drifts > 0 {
		score -= float64(drifts) * 0.05 // each drift degrades score slightly
		if score < 0 {
			score = 0
		}
	}

	var level, rec, reason string
	switch {
	case score >= 0.9 && drifts == 0:
		level, rec, reason = "healthy", "proceed", "all operations succeeded"
	case score >= 0.7 || drifts > 0:
		level, rec = "degraded", "hold"
		if failures > 0 {
			reason = fmt.Sprintf("%d failure(s), %d drift(s) in window", failures, drifts)
		} else {
			reason = fmt.Sprintf("%d drift(s) detected in window", drifts)
		}
	default:
		level, rec, reason = "unhealthy", "rollback",
			fmt.Sprintf("%d failure(s) in window", failures)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"partition": partition,
		"intent":    intent,
		"overall_health": map[string]any{
			"score":    score,
			"level":    level,
			"reason":   reason,
			"check_at": latestEvent.UTC(),
		},
		"recent_failures": failures,
		"recent_drifts":   drifts,
		"recommendation":  rec,
		"window":          windowStr,
		"computed_at":     time.Now().UTC(),
	})
}

func (s *Service) handleGuardianOverview(w http.ResponseWriter, r *http.Request) {
	guardianURL := s.guardianAPIBaseURL()
	if guardianURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "guardian url not configured",
		})
		return
	}

	resp, err := s.guardianGET(r.Context(), "/api/m2m/overview", "/api/overview")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "guardian unreachable",
		})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "failed to read guardian response",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func (s *Service) tenantFromRequest(r *http.Request) string {
	tenant := requestParam(r, "tenant")
	if tenant == "" && r != nil && s.tenantHeader != "" {
		tenant = strings.TrimSpace(r.Header.Get(s.tenantHeader))
	}
	if tenant == "" {
		tenant = s.defaultTenant
	}
	if tenant == "" {
		return "default"
	}
	return tenant
}

// partitionFromRequest returns the optional canonical partition filter used by
// the UI and Grafana. Both "partition" and "guardian_partition" are accepted.
func partitionFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if value := requestParam(r, "partition"); value != "" {
		return value
	}
	return requestParam(r, "guardian_partition")
}

func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if unixSeconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(unixSeconds, 0).UTC()
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func parseLimit(value string, fallback, max int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	if parsed > max {
		return max
	}
	return parsed
}

func requestParam(r *http.Request, name string) string {
	if err := r.ParseForm(); err != nil {
		return strings.TrimSpace(r.URL.Query().Get(name))
	}
	return strings.TrimSpace(r.Form.Get(name))
}

func requestParams(r *http.Request, name string) []string {
	if err := r.ParseForm(); err != nil {
		return r.URL.Query()[name]
	}
	return r.Form[name]
}

func writeJSON(w http.ResponseWriter, statusCode int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Service) handleRecentTraces(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	identity := identityFromRequest(r)
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	from := parseTime(r.URL.Query().Get("from"))
	to := parseTime(r.URL.Query().Get("to"))
	limit := parseLimit(r.URL.Query().Get("limit"), 100, 500)
	cursor, err := decodeTraceCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if from.IsZero() {
		from = time.Now().UTC().Add(-1 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}

	summaries, next, hasMore, err := s.recentTracePage(r.Context(), tenant, service, identity, from, to, limit, cursor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"returned":    len(summaries),
		"traces":      summaries,
		"has_more":    hasMore,
		"next_cursor": encodeTraceCursor(next),
		"page_size":   limit,
	})
}

func (s *Service) handleLogServices(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	partition := partitionFromRequest(r)
	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC()

	results, err := s.logEngine.QueryLogs(r.Context(), tenant, "", "", from, to, 2000)
	if err != nil {
		results = nil
	}

	seen := map[string]bool{}
	seenParts := map[string]bool{}
	seenIdentities := map[string]bool{}
	var components []string
	var partitions []string
	var identities []logIdentityInfo
	addIdentity := func(part, intent, asset, service string) {
		if part == "" && intent == "" && asset == "" {
			return
		}
		key := part + "\x00" + intent + "\x00" + asset
		if seenIdentities[key] {
			return
		}
		seenIdentities[key] = true
		identities = append(identities, logIdentityInfo{
			Partition: part,
			Intent:    intent,
			Asset:     asset,
			Service:   service,
		})
	}
	for _, rec := range results {
		attrs := mergeStringMaps(rec.ResourceAttributes, rec.Attributes)
		part, intent, asset := logIdentity(rec)
		if part != "" && !seenParts[part] {
			seenParts[part] = true
			partitions = append(partitions, part)
		}
		if partition != "" && part != partition {
			continue
		}
		comp := firstNonEmpty(rec.Service, attrs["guardian.component"], attrs["guardian_component"])
		if comp != "" && !seen[comp] {
			seen[comp] = true
			components = append(components, comp)
		}
		addIdentity(part, intent, asset, comp)
	}

	// Trace services may have no log records in the scan window. Include them
	// when the caller asks for a trace-oriented catalog so the traces page can
	// offer the same partition-scoped service filter as the logs page.
	include := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("include")))
	if strings.Contains(include, "traces") {
		if spans, spanErr := s.logEngine.QueryTraces(r.Context(), tenant, "", "", from, to, traceDiscoverySpanLimit); spanErr == nil {
			for _, span := range spans {
				part, intent, asset := traceIdentityLabels(span)
				if part != "" && !seenParts[part] {
					seenParts[part] = true
					partitions = append(partitions, part)
				}
				if partition != "" && part != partition {
					continue
				}
				if span.Service != "" && !seen[span.Service] {
					seen[span.Service] = true
					components = append(components, span.Service)
				}
				addIdentity(part, intent, asset, span.Service)
			}
		}
	}

	// Federated X-Ray services have no local log records, but should still be
	// selectable alongside native services and carry their AWS partition.
	for _, svc := range s.awsServiceInfos(r.Context()) {
		part, allowed := s.policyPartitionAllowed(r.Context(), svc.Account, svc.Region,
			awsPartition(svc.Region, svc.Account), policy.SignalLogs)
		if !allowed {
			continue
		}
		if part != "" && !seenParts[part] {
			seenParts[part] = true
			partitions = append(partitions, part)
		}
		if partition != "" && part != partition {
			continue
		}
		if svc.Name != "" && !seen[svc.Name] {
			seen[svc.Name] = true
			components = append(components, svc.Name)
		}
		addIdentity(part, "", "", svc.Name)
	}
	logGroups := make([]string, 0)
	if strings.Contains(include, "aws") && s.awsDiscoveryReady(r.Context()) {
		awsCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		for _, group := range proxy.AWSLogGroups(awsCtx, s.aws) {
			groupPartition := proxy.LogGroupPartition(group)
			if groupPartition == "" {
				continue
			}
			if partition != "" && groupPartition != partition {
				continue
			}
			if _, allowed := s.policyPartitionAllowed(r.Context(), "", "", groupPartition, policy.SignalLogs); !allowed {
				continue
			}
			logGroups = append(logGroups, group)
		}
	}
	sort.Strings(components)
	sort.Strings(partitions)
	sort.Strings(logGroups)
	sort.Slice(identities, func(i, j int) bool {
		left, right := identities[i], identities[j]
		if left.Partition != right.Partition {
			return left.Partition < right.Partition
		}
		if left.Intent != right.Intent {
			return left.Intent < right.Intent
		}
		if left.Asset != right.Asset {
			return left.Asset < right.Asset
		}
		return left.Service < right.Service
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":     tenant,
		"services":   components,
		"partitions": partitions,
		"identities": identities,
		"log_groups": logGroups,
	})
}

type logIdentityInfo struct {
	Partition string `json:"partition,omitempty"`
	Intent    string `json:"intent,omitempty"`
	Asset     string `json:"asset,omitempty"`
	Service   string `json:"service,omitempty"`
}

type svcInfo struct {
	hasTraces           bool
	hasMetrics          bool
	hasLogs             bool
	hasRuntimeMetrics   bool
	hasAssetObservation bool
	fullName            string
	alias               string
	assetType           string
	identityKind        string
	partition           string
	intent              string
	asset               string
	cluster             string
	pusher              string
	truncated           bool
	lastSeen            time.Time
	traceIDs            map[string]struct{}
	traceErrorIDs       map[string]struct{}
	traceSpanCount      int
	traceErrorCount     int
	logErrorCount       int
	metricNames         map[string]struct{}
}

type discoveredService struct {
	// Name is the canonical service identity emitted by the query service.
	Name          string   `json:"name"`
	FullName      string   `json:"full_name,omitempty"`
	Alias         string   `json:"alias,omitempty"`
	Kind          string   `json:"kind"`
	AssetType     string   `json:"asset_type,omitempty"`
	IdentityKind  string   `json:"identity_kind,omitempty"`
	Runtime       bool     `json:"runtime"`
	Truncated     bool     `json:"truncated,omitempty"`
	HasMetrics    bool     `json:"has_metrics"`
	HasTraces     bool     `json:"has_traces"`
	HasLogs       bool     `json:"has_logs"`
	MetricNames   []string `json:"metric_names,omitempty"`
	Partition     string   `json:"partition,omitempty"`
	Intent        string   `json:"intent,omitempty"`
	Asset         string   `json:"asset,omitempty"`
	Cluster       string   `json:"cluster,omitempty"`
	Pusher        string   `json:"pusher,omitempty"`
	LastSeen      string   `json:"last_seen"`
	TraceCount    int      `json:"trace_count"`
	LogCount      int      `json:"log_count"`
	ErrorRate     float64  `json:"error_rate"`
	SuggestedDash []string `json:"suggested_dashboards"`
}

// filterVisibleServices scopes the APM service catalog to the caller's
// partitions. Metrics remain globally readable through the metrics APIs; this
// prevents the catalog from exposing service identities from other
// partitions.
func (s *Service) filterVisibleServices(ctx context.Context, services []discoveredService) []discoveredService {
	if s.policyGate == nil || !s.policyGate.Enforced() {
		return services
	}
	id := identityFromContext(ctx)
	kept := make([]discoveredService, 0, len(services))
	for _, svc := range services {
		if s.policyGate.CanSeePartition(id, svc.Partition) {
			kept = append(kept, svc)
		}
	}
	return kept
}

type serviceSuggestion struct {
	Label       string `json:"label"`
	Query       string `json:"query"`
	Description string `json:"description"`
	Type        string `json:"type"` // "rate", "latency", "errors", "saturation"
	Window      string `json:"window"`
}

func mergeServiceIdentity(info *svcInfo, primary, secondary map[string]string, partition, intent, asset string) {
	if info == nil {
		return
	}
	if partition != "" {
		info.partition = partition
	}
	if intent != "" {
		info.intent = intent
	}
	if asset != "" {
		info.asset = asset
	}
	attr := func(key string) string {
		if value := strings.TrimSpace(primary[key]); value != "" {
			return value
		}
		return strings.TrimSpace(secondary[key])
	}
	if fullName := firstNonEmpty(attr(attrGuardianFullName), attr(labelServiceFull)); fullName != "" {
		info.fullName = fullName
	} else if info.partition != "" && info.intent != "" && info.asset != "" {
		info.fullName = info.partition + "." + info.intent + "." + info.asset
	}
	if alias := firstNonEmpty(attr(attrServiceAlias), attr(labelServiceAlias)); alias != "" {
		info.alias = alias
	}
	if assetType := firstNonEmpty(attr("guardian.asset.type"), attr("guardian_asset_type")); assetType != "" {
		info.assetType = assetType
	}
	if identityKind := firstNonEmpty(attr("guardian.identity.kind"), attr("guardian_identity_kind")); identityKind != "" {
		info.identityKind = identityKind
	}
	if cluster := firstNonEmpty(attr("guardian.cluster"), attr("k8s.cluster.name")); cluster != "" {
		info.cluster = cluster
	}
	if pusher := attr("guardian.pusher"); pusher != "" {
		info.pusher = pusher
	}
	if truncated := attr("guardian.service.truncated"); truncated != "" {
		info.truncated = strings.EqualFold(truncated, "true")
	}
}

func (s *Service) handleServiceDiscovery(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	window := parseWindow(r.URL.Query().Get("window"))
	if window == 0 {
		window = 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-window)

	svcMap := make(map[string]*svcInfo)
	getInfo := func(name string) *svcInfo {
		info, ok := svcMap[name]
		if !ok {
			info = newSvcInfo()
			svcMap[name] = info
		}
		return info
	}

	// Collect services from traces.
	traces, err := s.logEngine.QueryTraces(r.Context(), tenant, "", "", cutoff, time.Now().UTC(), 5000)
	if err != nil {
		traces = nil
	}
	for _, t := range traces {
		if t.Service == "" {
			continue
		}
		info := getInfo(t.Service)
		info.hasTraces = true
		info.traceSpanCount++
		if t.TraceID != "" {
			info.traceIDs[t.TraceID] = struct{}{}
			if traceStatusIsError(t.StatusCode) {
				info.traceErrorIDs[t.TraceID] = struct{}{}
			}
		} else if traceStatusIsError(t.StatusCode) {
			info.traceErrorCount++
		}
		partition, intent, asset := traceIdentityLabels(t)
		mergeServiceIdentity(info, t.ResourceAttributes, t.Attributes, partition, intent, asset)
		if t.EndTime.After(info.lastSeen) {
			info.lastSeen = t.EndTime
		}
	}

	// Collect services from logs.
	logs, err := s.logEngine.QueryLogs(r.Context(), tenant, "", "", cutoff, time.Now().UTC(), 5000)
	if err != nil {
		logs = nil
	}
	logCountBySvc := make(map[string]int)
	errorCountBySvc := make(map[string]int)
	totalLogs := 0
	for _, l := range logs {
		if l.Service == "" {
			continue
		}
		info := getInfo(l.Service)
		info.hasLogs = true
		partition, intent, asset := logIdentity(l)
		mergeServiceIdentity(info, l.ResourceAttributes, l.Attributes, partition, intent, asset)
		logCountBySvc[l.Service]++
		totalLogs++
		if l.SeverityText == "ERROR" || l.SeverityText == "FATAL" || l.SeverityNumber >= 17 {
			errorCountBySvc[l.Service]++
			info.logErrorCount++
		}
		if l.Timestamp.After(info.lastSeen) {
			info.lastSeen = l.Timestamp
		}
	}

	// Discover metric names per service via Prometheus labels. Stream the
	// records so an unbounded number of samples cannot exhaust memory.
	end := time.Now().UTC()
	start := end.Add(-window)
	scanned := 0
	_ = s.logEngine.StreamMetrics(r.Context(), tenant, segment.MetricQuery{}, start, end, func(m segment.MetricPointRecord) error {
		if scanned >= maxDiscoveryScannedRecords {
			return errDiscoveryScanLimit
		}
		scanned++
		if m.Service == "" {
			return nil
		}
		info := getInfo(m.Service)
		info.hasMetrics = true
		partition, intent, asset := metricIdentity(m)
		mergeServiceIdentity(info, m.ResourceAttributes, m.Attributes, partition, intent, asset)
		if m.MetricName == assetObservationMetric {
			info.hasAssetObservation = true
			// Asset observation describes the target, so its point-level alias
			// and type win over the producer's resource metadata.
			info.alias = firstNonEmpty(m.Attributes["service.alias"], m.Attributes["service_alias"])
			info.identityKind = "managed"
			// Asset observation describes the target, so its point-level type
			// wins over the producer's resource type.
			if assetType := firstNonEmpty(
				m.Attributes["guardian_asset_type"],
				m.Attributes["guardian.asset.type"],
				info.assetType,
			); assetType != "" {
				info.assetType = assetType
			}
			if info.partition != "" && info.intent != "" && info.asset != "" {
				info.fullName = info.partition + "." + info.intent + "." + info.asset
			}
		} else {
			info.hasRuntimeMetrics = true
		}
		if m.MetricName != "" {
			info.metricNames[m.MetricName] = struct{}{}
		}
		if m.Timestamp.After(info.lastSeen) {
			info.lastSeen = m.Timestamp
		}
		return nil
	})

	// Federated X-Ray services are trace-only signals; add them (with their AWS
	// partition) so AWS workloads show up next to native services.
	for _, svc := range s.awsServiceInfos(r.Context()) {
		if svc.Name == "" {
			continue
		}
		info := getInfo(svc.Name)
		info.hasTraces = true
		partition := awsPartition(svc.Region, svc.Account)
		if s.policyGate != nil {
			partition = s.policyGate.CanonicalPartition(svc.Account, svc.Region, partition)
		}
		if partition != "" {
			info.partition = partition
		}
	}

	includeAssets := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_assets")), "true")
	services := make([]discoveredService, 0, len(svcMap))
	for name, info := range svcMap {
		metricNames := make([]string, 0, len(info.metricNames))
		for mn := range info.metricNames {
			metricNames = append(metricNames, mn)
		}
		sort.Strings(metricNames)

		kind := classifyServiceKind(info)
		if kind == serviceKindAsset && !includeAssets {
			continue
		}
		suggested := []string{}
		if info.hasTraces || info.hasLogs || info.hasMetrics {
			suggested = append(suggested, "red")
		}
		if info.hasMetrics {
			suggested = append(suggested, "use")
		}

		traceCount := len(info.traceIDs)
		if traceCount == 0 {
			traceCount = info.traceSpanCount
		}
		errorRate := 0.0
		if totalLogs > 0 && logCountBySvc[name] > 0 {
			errorRate = float64(errorCountBySvc[name]) / float64(logCountBySvc[name])
		} else if traceCount > 0 {
			errorTraces := len(info.traceErrorIDs) + info.traceErrorCount
			errorRate = float64(errorTraces) / float64(traceCount)
			if errorRate > 1 {
				errorRate = 1
			}
		}

		fullName := info.fullName
		if fullName == "" {
			fullName = name
		}
		partition := info.partition
		if s.policyGate != nil && partition != "" {
			partition = s.policyGate.CanonicalPartition("", "", partition)
		}
		services = append(services, discoveredService{
			Name:          name,
			FullName:      fullName,
			Alias:         info.alias,
			Kind:          kind,
			AssetType:     info.assetType,
			IdentityKind:  info.identityKind,
			Runtime:       kind == serviceKindRuntime,
			Truncated:     info.truncated,
			HasMetrics:    info.hasMetrics,
			HasTraces:     info.hasTraces,
			HasLogs:       info.hasLogs,
			MetricNames:   metricNames,
			Partition:     partition,
			Intent:        info.intent,
			Asset:         info.asset,
			Cluster:       info.cluster,
			Pusher:        info.pusher,
			LastSeen:      formatRFC3339OrEmpty(info.lastSeen),
			TraceCount:    traceCount,
			LogCount:      logCountBySvc[name],
			ErrorRate:     errorRate,
			SuggestedDash: suggested,
		})
	}
	services = s.filterVisibleServices(r.Context(), services)

	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":         tenant,
		"window":         window.String(),
		"services":       services,
		"total_services": len(services),
		"include_assets": includeAssets,
		"discovered_at":  time.Now().UTC(),
	})
}

const (
	serviceKindRuntime     = "runtime"
	serviceKindAsset       = "asset"
	assetObservationMetric = "guardian_asset_observation_current"
)

func newSvcInfo() *svcInfo {
	return &svcInfo{
		metricNames:   make(map[string]struct{}),
		traceIDs:      make(map[string]struct{}),
		traceErrorIDs: make(map[string]struct{}),
	}
}

func classifyServiceKind(info *svcInfo) string {
	if info == nil {
		return serviceKindAsset
	}
	switch info.assetType {
	case "Compute", "LoadBalancer":
		return serviceKindRuntime
	case "":
		// No asset type (older producers or external services). Presence of
		// runtime signals is a safe fallback; an observation-only series is an
		// asset by default.
		if info.hasTraces || info.hasLogs || info.hasRuntimeMetrics {
			return serviceKindRuntime
		}
		return serviceKindAsset
	default:
		if info.hasTraces || info.hasLogs || info.hasRuntimeMetrics {
			return serviceKindRuntime
		}
		return serviceKindAsset
	}
}

func traceStatusIsError(status string) bool {
	status = strings.ToUpper(strings.TrimSpace(status))
	return strings.Contains(status, "ERROR")
}

func (s *Service) handleServiceSuggest(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	serviceName := strings.TrimSpace(r.URL.Query().Get("service"))
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "5m"
	}

	if serviceName == "" {
		http.Error(w, "service query param is required", http.StatusBadRequest)
		return
	}

	serviceRegex := regexp.QuoteMeta(serviceName)
	suggestions := []serviceSuggestion{
		{
			Label:       "Request Rate",
			Query:       fmt.Sprintf(`sum(rate(http_requests_total{service=~"%s"}[%s]))`, serviceRegex, window),
			Description: "Total requests per second",
			Type:        "rate",
			Window:      window,
		},
		{
			Label:       "Error Rate",
			Query:       fmt.Sprintf(`sum(rate(http_requests_total{service=~"%s",status_code=~"5.."}[%s]))`, serviceRegex, window),
			Description: "5xx errors per second",
			Type:        "errors",
			Window:      window,
		},
		{
			Label:       "Error Ratio",
			Query:       fmt.Sprintf(`sum(rate(http_requests_total{service=~"%s",status_code=~"5.."}[%s])) / sum(rate(http_requests_total{service=~"%s"}[%s]))`, serviceRegex, window, serviceRegex, window),
			Description: "Ratio of 5xx errors to total requests",
			Type:        "errors",
			Window:      window,
		},
		{
			Label:       "Latency p50",
			Query:       fmt.Sprintf(`histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket{service=~"%s"}[%s])))`, serviceRegex, window),
			Description: "Median request duration",
			Type:        "latency",
			Window:      window,
		},
		{
			Label:       "Latency p95",
			Query:       fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service=~"%s"}[%s])))`, serviceRegex, window),
			Description: "95th percentile request duration",
			Type:        "latency",
			Window:      window,
		},
		{
			Label:       "Latency p99",
			Query:       fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{service=~"%s"}[%s])))`, serviceRegex, window),
			Description: "99th percentile request duration",
			Type:        "latency",
			Window:      window,
		},
		{
			Label:       "Log Volume",
			Query:       fmt.Sprintf(`sum(rate(log_records_total{service=~"%s"}[%s]))`, serviceRegex, window),
			Description: "Log records per second",
			Type:        "saturation",
			Window:      window,
		},
		{
			Label:       "Error Logs",
			Query:       fmt.Sprintf(`sum(rate(log_records_total{service=~"%s",severity=~"ERROR|FATAL"}[%s]))`, serviceRegex, window),
			Description: "Error/fatal log rate",
			Type:        "errors",
			Window:      window,
		},
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":       tenant,
		"service":      serviceName,
		"window":       window,
		"suggestions":  suggestions,
		"generated_at": time.Now().UTC(),
	})
}
