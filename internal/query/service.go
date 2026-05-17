package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	monofsstore "github.com/rydzu/ainfra/doctor/internal/store/monofs"
)

// LogEngineBackend is the interface used for logs, metrics, and trace queries.
type LogEngineBackend interface {
	QueryLogs(ctx context.Context, tenant, query, service string, from, to time.Time, limit int) ([]segment.LogRecord, error)
	QueryMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time) ([]segment.MetricPointRecord, error)
	QueryTraces(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, error)
}

type Service struct {
	defaultTenant    string
	guardianURL      string
	guardianAPIURL   string
	defaultPartition string
	logEngine        LogEngineBackend
	guardianClient   *http.Client
}

// SetGuardianURL sets the URL of the Guardian web UI shown as a header link.
func (s *Service) SetGuardianURL(u string) { s.guardianURL = u }

// SetGuardianAPIURL sets the Guardian API base URL used for server-side fetches.
func (s *Service) SetGuardianAPIURL(u string) { s.guardianAPIURL = u }

// SetDefaultPartition sets the partition pre-filled in the topology form.
func (s *Service) SetDefaultPartition(p string) { s.defaultPartition = p }

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
	Intent        string            `json:"intent,omitempty"`
	Asset         string            `json:"asset,omitempty"`
	AssetType     string            `json:"assetType,omitempty"`
	Status        string            `json:"status"`
	DisplayStatus string            `json:"displayStatus"`
	Health        string            `json:"health"`
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
	url := fmt.Sprintf("%s/api/partitions/%s/history", strings.TrimRight(guardianURL, "/"), partition)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.guardianClient.Do(req)
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
	url := fmt.Sprintf("%s/api/partitions", strings.TrimRight(guardianURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.guardianClient.Do(req)
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
	url := fmt.Sprintf("%s/api/partitions/%s/topology", strings.TrimRight(guardianURL, "/"), partition)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.guardianClient.Do(req)
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
	mux.HandleFunc("/api/v1/status/buildinfo", s.handlePrometheusBuildInfo)
	mux.HandleFunc("/loki/api/v1/query", s.handleLokiQuery)
	mux.HandleFunc("/loki/api/v1/query_range", s.handleLokiQueryRange)
	mux.HandleFunc("/loki/api/v1/labels", s.handleLokiLabels)
	mux.HandleFunc("/loki/api/v1/label/", s.handleLokiLabelValues)
	mux.HandleFunc("/loki/api/v1/series", s.handleLokiSeries)
	mux.HandleFunc("/api/v2/services", s.handleZipkinServices)
	mux.HandleFunc("/api/v2/spans", s.handleZipkinSpans)
	mux.HandleFunc("/api/v2/traces", s.handleZipkinTraces)
	mux.HandleFunc("/api/v2/trace/", s.handleZipkinTraceByID)
	mux.HandleFunc("/v1/spans/search", s.handleSpanSearch)
	mux.HandleFunc("/v1/traces/recent", s.handleRecentTraces)
	mux.HandleFunc("/v1/traces/", s.handleTraceByID)
	mux.HandleFunc("/v1/logs/services", s.handleLogServices)
	mux.HandleFunc("/v1/logs/search", s.handleLogSearch)
	mux.HandleFunc("/v1/metrics/range", s.handleMetricRange)
	mux.HandleFunc("/v1/guardian/topology", s.handleGuardianTopology)
	mux.HandleFunc("/v1/guardian/events", s.handleGuardianEvents)
	mux.HandleFunc("/v1/guardian/health", s.handleGuardianHealth)
	mux.HandleFunc("/v1/ui/config", s.handleUIConfig)
	mux.HandleFunc("/", s.handleUI)
	return mux
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

func (s *Service) handleTraceByID(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimPrefix(r.URL.Path, "/v1/traces/")
	if traceID == "" {
		http.Error(w, "trace id is required", http.StatusBadRequest)
		return
	}
	tenant := s.tenantFromRequest(r)
	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC()
	spans, err := s.logEngine.QueryTraces(r.Context(), tenant, traceID, "", from, to, 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(spans) == 0 {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
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

	spans, err := s.logEngine.QueryTraces(r.Context(), tenant, traceID, service, from, to, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	from := parseTime(r.URL.Query().Get("from"))
	to := parseTime(r.URL.Query().Get("to"))
	limit := parseLimit(r.URL.Query().Get("limit"), 100, 1000)

	if from.IsZero() {
		from = time.Now().UTC().Add(-1 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}

	results, err := s.logEngine.QueryLogs(r.Context(), tenant, query, service, from, to, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Timestamp.After(results[j].Timestamp) })

	writeJSON(w, http.StatusOK, map[string]any{
		"service":  service,
		"query":    query,
		"returned": len(results),
		"results":  results,
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
	points, err := s.logEngine.QueryMetrics(r.Context(), tenant, segment.MetricQuery{MetricName: metricName}, from, to)
	if err != nil {
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

	if partition == "" || s.guardianAPIBaseURL() == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant":    tenant,
			"partition": partition,
			"nodes":     []any{},
			"edges":     []any{},
			"built_at":  time.Now().UTC(),
		})
		return
	}

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

	// Translate guardian TopologyNode → doctor DeploymentNode shape.
	type outNode struct {
		ID           string            `json:"id"`
		Type         string            `json:"type"`
		Name         string            `json:"name"`
		Partition    string            `json:"partition"`
		Intent       string            `json:"intent,omitempty"`
		Status       string            `json:"status"`
		TargetPusher string            `json:"target_pusher,omitempty"`
		LastEventAt  string            `json:"last_event_at"`
		Health       map[string]any    `json:"health"`
		Meta         map[string]string `json:"meta,omitempty"`
	}
	type outEdge struct {
		From     string `json:"from"`
		To       string `json:"to"`
		Relation string `json:"relation"`
	}

	nodes := make([]outNode, 0, len(topo.Nodes))
	for _, n := range topo.Nodes {
		healthLevel := n.Health
		if healthLevel == "" {
			healthLevel = "unknown"
		}
		nodes = append(nodes, outNode{
			ID:        n.ID,
			Type:      n.Kind,
			Name:      n.Label,
			Partition: partition,
			Intent:    n.Intent,
			Status:    n.Status,
			Health: map[string]any{
				"score": 0.5,
				"level": healthLevel,
			},
			Meta: n.Meta,
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

func (s *Service) tenantFromRequest(r *http.Request) string {
	tenant := requestParam(r, "tenant")
	if tenant == "" {
		tenant = s.defaultTenant
	}
	if tenant == "" {
		return "default"
	}
	return tenant
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
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	from := parseTime(r.URL.Query().Get("from"))
	to := parseTime(r.URL.Query().Get("to"))
	limit := parseLimit(r.URL.Query().Get("limit"), 100, 500)

	if from.IsZero() {
		from = time.Now().UTC().Add(-1 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}

	// Recent trace summaries must be built from the full matching span set.
	// Sampling a fixed number of spans can hide valid traces when a few traces
	// contribute many spans inside the same window.
	spans, err := s.logEngine.QueryTraces(r.Context(), tenant, "", service, from, to, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type TraceSummary struct {
		TraceID   string    `json:"trace_id"`
		Service   string    `json:"service"`
		MinTime   time.Time `json:"min_time"`
		MaxTime   time.Time `json:"max_time"`
		SpanCount int       `json:"span_count"`
	}

	traceMap := make(map[string]*TraceSummary)
	for _, span := range spans {
		if span.TraceID == "" {
			continue
		}
		t, ok := traceMap[span.TraceID]
		if !ok {
			t = &TraceSummary{
				TraceID: span.TraceID,
				Service: span.Service,
				MinTime: span.StartTime,
				MaxTime: span.EndTime,
			}
			traceMap[span.TraceID] = t
		}
		t.SpanCount++
		if span.StartTime.Before(t.MinTime) {
			t.MinTime = span.StartTime
		}
		if span.EndTime.After(t.MaxTime) {
			t.MaxTime = span.EndTime
		}
	}

	summaries := make([]TraceSummary, 0, len(traceMap))
	for _, t := range traceMap {
		summaries = append(summaries, *t)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].MaxTime.After(summaries[j].MaxTime)
	})
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"returned": len(summaries),
		"traces":   summaries,
	})
}

func (s *Service) handleLogServices(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":   tenant,
		"services": []string{},
	})
}
