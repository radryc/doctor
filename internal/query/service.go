package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
	paths := []string{m2mPath, legacyPath}
	for i, p := range paths {
		url := base + p
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
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
	mux.HandleFunc("/v1/services/discover", s.handleServiceDiscovery)
	mux.HandleFunc("/v1/services/suggest", s.handleServiceSuggest)
	mux.HandleFunc("/v1/guardian/topology", s.handleGuardianTopology)
	mux.HandleFunc("/v1/guardian/events", s.handleGuardianEvents)
	mux.HandleFunc("/v1/guardian/health", s.handleGuardianHealth)
	mux.HandleFunc("/v1/guardian/overview", s.handleGuardianOverview)
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

	type outNode struct {
		ID            string            `json:"id"`
		Type          string            `json:"type"`
		Name          string            `json:"name"`
		Partition     string            `json:"partition"`
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
		Partition string    `json:"partition,omitempty"`
		Intent    string    `json:"intent,omitempty"`
		Asset     string    `json:"asset,omitempty"`
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
				TraceID:   span.TraceID,
				Service:   span.Service,
				Partition: span.Partition,
				Intent:    span.Intent,
				Asset:     span.Asset,
				MinTime:   span.StartTime,
				MaxTime:   span.EndTime,
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
	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC()

	results, err := s.logEngine.QueryLogs(r.Context(), tenant, "", "", from, to, 2000)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant":     tenant,
			"services":   []string{},
			"partitions": []string{},
		})
		return
	}

	seen := map[string]bool{}
	seenParts := map[string]bool{}
	var components []string
	var partitions []string
	for _, rec := range results {
		comp := rec.ResourceAttributes["guardian.component"]
		if comp == "" {
			comp = rec.Service
		}
		if comp != "" && !seen[comp] {
			seen[comp] = true
			components = append(components, comp)
		}
		part := rec.Partition
		if part == "" {
			part = rec.ResourceAttributes["guardian.partition"]
		}
		if part != "" && !seenParts[part] {
			seenParts[part] = true
			partitions = append(partitions, part)
		}
	}
	sort.Strings(components)
	sort.Strings(partitions)

	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":     tenant,
		"services":   components,
		"partitions": partitions,
	})
}

type discoveredService struct {
	Name           string   `json:"name"`
	HasMetrics     bool     `json:"has_metrics"`
	HasTraces      bool     `json:"has_traces"`
	HasLogs        bool     `json:"has_logs"`
	MetricNames    []string `json:"metric_names,omitempty"`
	Partition      string   `json:"partition,omitempty"`
	Intent         string   `json:"intent,omitempty"`
	LastSeen       string   `json:"last_seen"`
	TraceCount     int      `json:"trace_count"`
	LogCount       int      `json:"log_count"`
	ErrorRate      float64  `json:"error_rate,omitempty"`
	SuggestedDash  []string `json:"suggested_dashboards"`
}

type serviceSuggestion struct {
	Label       string `json:"label"`
	Query       string `json:"query"`
	Description string `json:"description"`
	Type        string `json:"type"` // "rate", "latency", "errors", "saturation"
	Window      string `json:"window"`
}

func (s *Service) handleServiceDiscovery(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	window := parseWindow(r.URL.Query().Get("window"))
	if window == 0 {
		window = 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-window)

	// Collect services from traces.
	traces, err := s.logEngine.QueryTraces(r.Context(), tenant, "", "", cutoff, time.Now().UTC(), 5000)
	if err != nil {
		traces = nil
	}

	type svcInfo struct {
		hasTraces   bool
		hasMetrics  bool
		hasLogs     bool
		partition   string
		intent      string
		lastSeen    time.Time
		traceCount  int
		metricNames map[string]struct{}
	}

	svcMap := make(map[string]*svcInfo)
	for _, t := range traces {
		if t.Service == "" {
			continue
		}
		info, ok := svcMap[t.Service]
		if !ok {
			info = &svcInfo{metricNames: make(map[string]struct{})}
			svcMap[t.Service] = info
		}
		info.hasTraces = true
		info.traceCount++
		if t.Partition != "" {
			info.partition = t.Partition
		}
		if t.Intent != "" {
			info.intent = t.Intent
		}
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
		info, ok := svcMap[l.Service]
		if !ok {
			info = &svcInfo{metricNames: make(map[string]struct{})}
			svcMap[l.Service] = info
		}
		info.hasLogs = true
		logCountBySvc[l.Service]++
		totalLogs++
		if l.SeverityText == "ERROR" || l.SeverityText == "FATAL" || l.SeverityNumber >= 17 {
			errorCountBySvc[l.Service]++
		}
		if l.Timestamp.After(info.lastSeen) {
			info.lastSeen = l.Timestamp
		}
	}

	// Discover metric names per service via Prometheus labels.
	end := time.Now().UTC()
	start := end.Add(-window)
	metricNamesResp, err := s.logEngine.QueryMetrics(r.Context(), tenant, segment.MetricQuery{}, start, end)
	if err == nil {
		seenMetrics := make(map[string]struct{})
		for _, m := range metricNamesResp {
			seenMetrics[m.MetricName] = struct{}{}
			if m.Service != "" {
				info, ok := svcMap[m.Service]
				if !ok {
					info = &svcInfo{metricNames: make(map[string]struct{})}
					svcMap[m.Service] = info
				}
				info.hasMetrics = true
				info.metricNames[m.MetricName] = struct{}{}
				if m.Timestamp.After(info.lastSeen) {
					info.lastSeen = m.Timestamp
				}
			}
		}
	}

	// Build response.
	services := make([]discoveredService, 0, len(svcMap))
	for name, info := range svcMap {
		metricNames := make([]string, 0, len(info.metricNames))
		for mn := range info.metricNames {
			metricNames = append(metricNames, mn)
		}
		sort.Strings(metricNames)

		suggested := []string{}
		if info.hasTraces || info.hasLogs || info.hasMetrics {
			suggested = append(suggested, "red")
		}
		if info.hasMetrics {
			suggested = append(suggested, "use")
		}

		errorRate := 0.0
		if totalLogs > 0 && logCountBySvc[name] > 0 {
			errorRate = float64(errorCountBySvc[name]) / float64(logCountBySvc[name])
		}

		services = append(services, discoveredService{
			Name:          name,
			HasMetrics:    info.hasMetrics,
			HasTraces:     info.hasTraces,
			HasLogs:       info.hasLogs,
			MetricNames:   metricNames,
			Partition:     info.partition,
			Intent:        info.intent,
			LastSeen:      formatRFC3339OrEmpty(info.lastSeen),
			TraceCount:    info.traceCount,
			LogCount:      logCountBySvc[name],
			ErrorRate:     errorRate,
			SuggestedDash: suggested,
		})
	}

	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":           tenant,
		"window":           window.String(),
		"services":         services,
		"total_services":   len(services),
		"discovered_at":    time.Now().UTC(),
	})
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
		"tenant":     tenant,
		"service":    serviceName,
		"window":     window,
		"suggestions": suggestions,
		"generated_at": time.Now().UTC(),
	})
}
