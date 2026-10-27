package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/buildinfo"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

func TestHandleGuardianHealthUsesLivePartitionFallbackWithoutRecentEvents(t *testing.T) {
	t.Helper()

	guardian := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/partitions":
			writeJSON(w, http.StatusOK, []map[string]any{{
				"name":          "doctor",
				"status":        "Attention",
				"displayStatus": "Attention",
				"health":        "attention",
			}})
		case "/api/partitions/doctor/history":
			writeJSON(w, http.StatusOK, map[string]any{
				"generatedAt": time.Now().UTC(),
				"events":      []any{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer guardian.Close()

	svc := &Service{
		defaultTenant:  "default",
		guardianURL:    guardian.URL,
		guardianClient: guardian.Client(),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/guardian/health?partition=doctor&window=1h", nil)
	rec := httptest.NewRecorder()

	svc.handleGuardianHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Partition      string `json:"partition"`
		Recommendation string `json:"recommendation"`
		OverallHealth  struct {
			Level  string  `json:"level"`
			Score  float64 `json:"score"`
			Reason string  `json:"reason"`
		} `json:"overall_health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Partition; got != "doctor" {
		t.Fatalf("partition = %q, want doctor", got)
	}
	if got := resp.OverallHealth.Level; got != "attention" {
		t.Fatalf("level = %q, want attention", got)
	}
	if got := resp.OverallHealth.Score; got == 0.5 {
		t.Fatalf("score = %v, want live fallback score instead of unknown midpoint", got)
	}
	if got := resp.OverallHealth.Reason; got != "Attention" {
		t.Fatalf("reason = %q, want Attention", got)
	}
	if got := resp.Recommendation; got != "hold" {
		t.Fatalf("recommendation = %q, want hold", got)
	}
}

type fakeLogEngineBackend struct {
	logs            []segment.LogRecord
	metrics         []segment.MetricPointRecord
	traces          []segment.TraceRecord
	lastLimit       int
	lastMetricQuery segment.MetricQuery
	traceQueries    []fakeTraceQuery
}

type fakeTraceQuery struct {
	traceID string
	service string
	from    time.Time
	to      time.Time
	limit   int
}

const (
	fakeMetricDiscoveryMatcherName = "__doctor_discovery__"
	fakeMetricDiscoveryModeNames   = "metric_names"
)

func (f *fakeLogEngineBackend) QueryLogs(_ context.Context, _ string, query, service string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	var filtered []segment.LogRecord
	for _, record := range f.logs {
		if service != "" && record.Service != service {
			continue
		}
		if !from.IsZero() && record.Timestamp.Before(from) {
			continue
		}
		if !to.IsZero() && record.Timestamp.After(to) {
			continue
		}
		if query != "" && !strings.Contains(record.Body, query) {
			continue
		}
		filtered = append(filtered, record)
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (f *fakeLogEngineBackend) QueryLogsRaw(_ context.Context, _ string, _ string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	return f.QueryLogs(context.Background(), "", "", "", from, to, limit)
}

func (f *fakeLogEngineBackend) QueryMetrics(_ context.Context, _ string, query segment.MetricQuery, from, to time.Time) ([]segment.MetricPointRecord, error) {
	f.lastMetricQuery = query
	if fakeMetricDiscoveryMode(query) == fakeMetricDiscoveryModeNames {
		seen := make(map[string]struct{})
		filtered := make([]segment.MetricPointRecord, 0)
		for _, metric := range f.metrics {
			if metric.MetricName == "" {
				continue
			}
			if !from.IsZero() && metric.Timestamp.Before(from) {
				continue
			}
			if !to.IsZero() && metric.Timestamp.After(to) {
				continue
			}
			if _, ok := seen[metric.MetricName]; ok {
				continue
			}
			seen[metric.MetricName] = struct{}{}
			filtered = append(filtered, segment.MetricPointRecord{MetricName: metric.MetricName})
		}
		return filtered, nil
	}

	var filtered []segment.MetricPointRecord
	for _, metric := range f.metrics {
		if metric.MetricName == "" {
			continue
		}
		if from.IsZero() == false && metric.Timestamp.Before(from) {
			continue
		}
		if to.IsZero() == false && metric.Timestamp.After(to) {
			continue
		}
		if query.MetricName != "" && metric.MetricName != query.MetricName {
			continue
		}
		if query.Service != "" && metric.Service != query.Service {
			continue
		}
		if !fakeMetricLabelsMatch(metric, query.LabelMatchers) {
			continue
		}
		filtered = append(filtered, metric)
	}
	return filtered, nil
}

func (f *fakeLogEngineBackend) StreamMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricPointRecord) error) error {
	records, err := f.QueryMetrics(ctx, tenant, query, from, to)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := yield(record); err != nil {
			return err
		}
	}
	return nil
}

// StreamMetricSeries synthesizes the chunk series index from the fake's stored
// points: one entry per name/service/label-set with first/last sample times.
func (f *fakeLogEngineBackend) StreamMetricSeries(_ context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricSeriesMeta) error) error {
	type seriesKey struct {
		name string
		key  string
	}
	byKey := make(map[seriesKey]*segment.MetricSeriesMeta)
	order := make([]seriesKey, 0)
	for _, metric := range f.metrics {
		if metric.MetricName == "" || metric.Timestamp.IsZero() {
			continue
		}
		if !from.IsZero() && metric.Timestamp.Before(from) {
			continue
		}
		if !to.IsZero() && metric.Timestamp.After(to) {
			continue
		}
		if query.MetricName != "" && metric.MetricName != query.MetricName {
			continue
		}
		if query.Service != "" && metric.Service != query.Service {
			continue
		}
		if !fakeMetricLabelsMatch(metric, query.LabelMatchers) {
			continue
		}
		labelsKey := metricLabelMapString(metric.Attributes) + "\x01" + metricLabelMapString(metric.ResourceAttributes)
		key := seriesKey{name: metric.MetricName, key: metric.Service + "\x00" + labelsKey}
		entry := byKey[key]
		if entry == nil {
			labels := make(map[string]string, len(metric.Attributes)+len(metric.ResourceAttributes))
			for name, value := range metric.ResourceAttributes {
				labels[name] = value
			}
			for name, value := range metric.Attributes {
				labels[name] = value
			}
			entry = &segment.MetricSeriesMeta{
				Tenant:     tenant,
				Service:    metric.Service,
				MetricName: metric.MetricName,
				Labels:     labels,
				MinTime:    metric.Timestamp.UTC(),
				MaxTime:    metric.Timestamp.UTC(),
			}
			byKey[key] = entry
			order = append(order, key)
			continue
		}
		if metric.Timestamp.Before(entry.MinTime) {
			entry.MinTime = metric.Timestamp.UTC()
		}
		if metric.Timestamp.After(entry.MaxTime) {
			entry.MaxTime = metric.Timestamp.UTC()
		}
	}
	for _, key := range order {
		if err := yield(*byKey[key]); err != nil {
			return err
		}
	}
	return nil
}

func fakeMetricDiscoveryMode(query segment.MetricQuery) string {
	for _, matcher := range query.LabelMatchers {
		if matcher.Name == fakeMetricDiscoveryMatcherName && matcher.Type == segment.MetricMatchEqual {
			return matcher.Value
		}
	}
	return ""
}

func fakeMetricLabelsMatch(metric segment.MetricPointRecord, matchers []segment.MetricLabelMatcher) bool {
	for _, matcher := range matchers {
		var value string
		switch matcher.Name {
		case "__name__":
			value = metric.MetricName
		case "service":
			value = metric.Service
		default:
			value = metric.Attributes[matcher.Name]
			if value == "" {
				value = metric.ResourceAttributes[matcher.Name]
			}
		}
		switch matcher.Type {
		case segment.MetricMatchEqual:
			if value != matcher.Value {
				return false
			}
		case segment.MetricMatchNotEqual:
			if value == matcher.Value {
				return false
			}
		case segment.MetricMatchRegexp:
			if !strings.Contains(value, strings.Trim(matcher.Value, ".*")) {
				return false
			}
		case segment.MetricMatchNotRegexp:
			if strings.Contains(value, strings.Trim(matcher.Value, ".*")) {
				return false
			}
		}
	}
	return true
}

func (f *fakeLogEngineBackend) QueryTraces(_ context.Context, _, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, error) {
	f.lastLimit = limit
	f.traceQueries = append(f.traceQueries, fakeTraceQuery{
		traceID: traceID,
		service: service,
		from:    from,
		to:      to,
		limit:   limit,
	})

	var filtered []segment.TraceRecord
	for _, trace := range f.traces {
		if traceID != "" && trace.TraceID != traceID {
			continue
		}
		if service != "" && trace.Service != service {
			continue
		}
		if !from.IsZero() && trace.StartTime.Before(from) {
			continue
		}
		if !to.IsZero() && trace.StartTime.After(to) {
			continue
		}
		filtered = append(filtered, trace)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].StartTime.After(filtered[j].StartTime)
	})
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func TestHandleRecentTracesQueriesBoundedSpanSet(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	from := base.Add(-1 * time.Hour)
	to := base

	var spans []segment.TraceRecord
	for i := 0; i < 19; i++ {
		traceTime := base.Add(-time.Duration(i+1) * time.Minute)
		for j := 0; j < 11; j++ {
			spans = append(spans, segment.TraceRecord{
				TraceID:   "noisy-trace-" + string(rune('a'+i)),
				SpanID:    "span",
				Service:   "guardian",
				StartTime: traceTime,
				EndTime:   traceTime.Add(500 * time.Millisecond),
			})
		}
	}

	targetTime := base.Add(-20 * time.Minute)
	targetTraceID := "f9264f8346af4d77b21ff02af8aa0983"
	spans = append(spans, segment.TraceRecord{
		TraceID:   targetTraceID,
		SpanID:    "target-span",
		Service:   "guardian",
		StartTime: targetTime,
		EndTime:   targetTime.Add(750 * time.Millisecond),
	})

	backend := &fakeLogEngineBackend{traces: spans}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/v1/traces/recent?tenant=default&limit=20&from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()

	svc.handleRecentTraces(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if backend.lastLimit != traceDiscoverySpanLimit {
		t.Fatalf("QueryTraces limit = %d, want %d", backend.lastLimit, traceDiscoverySpanLimit)
	}

	var resp struct {
		Returned int `json:"returned"`
		Traces   []struct {
			TraceID string `json:"trace_id"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Returned != 20 {
		t.Fatalf("returned = %d, want 20", resp.Returned)
	}

	for _, trace := range resp.Traces {
		if trace.TraceID == targetTraceID {
			return
		}
	}
	t.Fatalf("response did not include trace %s", targetTraceID)
}

func TestHandleLogServicesFiltersServicesByPartition(t *testing.T) {
	base := time.Now().UTC()
	backend := &fakeLogEngineBackend{logs: []segment.LogRecord{
		{Tenant: "default", Service: "doctor.query", Partition: "doctor", Timestamp: base, Body: "ready"},
		{Tenant: "default", Service: "monofs.node", Partition: "monofs", Timestamp: base, Body: "ready"},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}
	req := httptest.NewRequest(http.MethodGet, "/v1/logs/services?tenant=default&partition=doctor", nil)
	rec := httptest.NewRecorder()
	svc.handleLogServices(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Services   []string `json:"services"`
		Partitions []string `json:"partitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Services) != 1 || resp.Services[0] != "doctor.query" {
		t.Fatalf("services = %v, want only doctor.query", resp.Services)
	}
	if len(resp.Partitions) != 2 {
		t.Fatalf("partitions = %v, want both partitions", resp.Partitions)
	}
}

func TestLogSearchPushdownUsesAWSLogGroup(t *testing.T) {
	pushdown := logSearchPushdown("", "", "guardian/123/us-east-1/demo-build/web")
	if !strings.Contains(pushdown.Selector, `source="aws"`) || !strings.Contains(pushdown.Selector, `aws_log_group="guardian/123/us-east-1/demo-build/web"`) {
		t.Fatalf("selector = %q", pushdown.Selector)
	}
}

func TestHandleLogSearchFiltersByPartition(t *testing.T) {
	base := time.Now().UTC()
	backend := &fakeLogEngineBackend{logs: []segment.LogRecord{
		{Tenant: "default", Service: "doctor.query", Partition: "doctor", Timestamp: base, Body: "ready"},
		{Tenant: "default", Service: "monofs.node", Partition: "monofs", Timestamp: base, Body: "ready"},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}
	req := httptest.NewRequest(http.MethodGet, "/v1/logs/search?tenant=default&partition=doctor&from="+url.QueryEscape(base.Add(-time.Hour).Format(time.RFC3339))+"&to="+url.QueryEscape(base.Add(time.Minute).Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()
	svc.handleLogSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []segment.LogRecord `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Service != "doctor.query" {
		t.Fatalf("results = %+v, want only doctor log", resp.Results)
	}
}

func TestHandleLogServicesIncludesFilteredAWSLogGroups(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/aws/log-groups" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"log_groups": []string{
			"guardian/123/us-east-1/demo-build/web",
			"guardian/123/us-east-1/demo-deploy/api",
		}})
	}))
	defer server.Close()
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	svc.SetAWSBackend(awsstore.NewClient(server.URL, ""))
	req := httptest.NewRequest(http.MethodGet, "/v1/logs/services?tenant=default&include=aws&partition=demo-build", nil)
	rec := httptest.NewRecorder()
	svc.handleLogServices(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		LogGroups []string `json:"log_groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.LogGroups) != 1 || resp.LogGroups[0] != "guardian/123/us-east-1/demo-build/web" {
		t.Fatalf("log groups = %v", resp.LogGroups)
	}
}

func TestHandleRecentTracesFiltersByPartition(t *testing.T) {
	base := time.Now().UTC()
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{
		{TraceID: "doctor-trace", SpanID: "s1", Service: "doctor.query", Partition: "doctor", StartTime: base.Add(-time.Minute), EndTime: base},
		{TraceID: "monofs-trace", SpanID: "s2", Service: "monofs.node", Partition: "monofs", StartTime: base.Add(-time.Minute), EndTime: base},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}
	req := httptest.NewRequest(http.MethodGet, "/v1/traces/recent?tenant=default&partition=doctor&from="+url.QueryEscape(base.Add(-time.Hour).Format(time.RFC3339))+"&to="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()
	svc.handleRecentTraces(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Traces []struct {
			TraceID string `json:"trace_id"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Traces) != 1 || resp.Traces[0].TraceID != "doctor-trace" {
		t.Fatalf("traces = %+v, want only doctor trace", resp.Traces)
	}
}

func TestHandlePrometheusQueryFiltersByPartition(t *testing.T) {
	base := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{
			{Tenant: "default", Service: "doctor.query", Partition: "doctor", MetricName: "demo_metric", Timestamp: base, HasValue: true, Value: 10},
			{Tenant: "default", Service: "monofs.node", Partition: "monofs", MetricName: "demo_metric", Timestamp: base, HasValue: true, Value: 20},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?tenant=default&partition=doctor&query=demo_metric&time="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result = %+v, want one partition series", resp.Data.Result)
	}
	if got := resp.Data.Result[0].Metric["guardian_partition"]; got != "doctor" {
		t.Fatalf("guardian_partition = %q, want doctor", got)
	}
}

func TestHandlePrometheusQueryReturnsVector(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{
			{
				Tenant:     "default",
				Service:    "api",
				MetricName: "http_requests_total",
				Timestamp:  base.Add(-1 * time.Minute),
				Attributes: map[string]string{"instance": "pod-a"},
				HasValue:   true,
				Value:      41,
			},
			{
				Tenant:     "default",
				Service:    "api",
				MetricName: "http_requests_total",
				Timestamp:  base,
				Attributes: map[string]string{"instance": "pod-a"},
				HasValue:   true,
				Value:      42,
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?tenant=default&query=http_requests_total&time="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()

	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if resp.Data.ResultType != "vector" {
		t.Fatalf("resultType = %q, want vector", resp.Data.ResultType)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1", len(resp.Data.Result))
	}
	if got := resp.Data.Result[0].Metric["__name__"]; got != "http_requests_total" {
		t.Fatalf("metric name = %q, want http_requests_total", got)
	}
	if got := resp.Data.Result[0].Metric["instance"]; got != "pod-a" {
		t.Fatalf("instance = %q, want pod-a", got)
	}
	if got := resp.Data.Result[0].Metric["service"]; got != "api" {
		t.Fatalf("service = %q, want api", got)
	}
	if len(resp.Data.Result[0].Value) != 2 {
		t.Fatalf("value payload length = %d, want 2", len(resp.Data.Result[0].Value))
	}
	if got, ok := resp.Data.Result[0].Value[1].(string); !ok || got != "42" {
		t.Fatalf("sample value = %#v, want \"42\"", resp.Data.Result[0].Value[1])
	}
}

func TestHandlePrometheusLabelValuesUsesMetricNameDiscovery(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{
			{
				Tenant:     "default",
				Service:    "api",
				MetricName: "http_requests_total",
				Timestamp:  base.Add(-1 * time.Minute),
				HasValue:   true,
				Value:      41,
			},
			{
				Tenant:     "default",
				Service:    "api",
				MetricName: "process_cpu_seconds_total",
				Timestamp:  base,
				HasValue:   true,
				Value:      2,
			},
			{
				Tenant:     "default",
				Service:    "api",
				MetricName: "http_requests_total",
				Timestamp:  base,
				HasValue:   true,
				Value:      42,
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/label/__name__/values?tenant=default&start="+url.QueryEscape(base.Add(-5*time.Minute).Format(time.RFC3339))+"&end="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()

	svc.handlePrometheusLabelValues(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := fakeMetricDiscoveryMode(backend.lastMetricQuery); got != fakeMetricDiscoveryModeNames {
		t.Fatalf("discovery mode = %q, want %q", got, fakeMetricDiscoveryModeNames)
	}

	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	want := []string{"http_requests_total", "process_cpu_seconds_total"}
	if len(resp.Data) != len(want) {
		t.Fatalf("data = %v, want %v", resp.Data, want)
	}
	for idx := range want {
		if resp.Data[idx] != want[idx] {
			t.Fatalf("data = %v, want %v", resp.Data, want)
		}
	}
}

func TestHandlePrometheusLabelValuesFiltersByPartition(t *testing.T) {
	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{
			{Tenant: "default", Service: "doctor.query", Partition: "doctor", MetricName: "doctor_ingest_requests_accepted", Timestamp: base, HasValue: true, Value: 1},
			{Tenant: "default", Service: "monofs.node", Partition: "monofs", MetricName: "monofs_server_ops_total", Timestamp: base, HasValue: true, Value: 1},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/label/__name__/values?tenant=default&partition=doctor&start="+url.QueryEscape(base.Add(-5*time.Minute).Format(time.RFC3339))+"&end="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()
	svc.handlePrometheusLabelValues(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0] != "doctor_ingest_requests_accepted" {
		t.Fatalf("data = %v, want only doctor metric", resp.Data)
	}
}

func TestHandlePrometheusLabelValuesNormalizesDottedMetricNames(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{
			{
				Tenant:     "default",
				Service:    "doctor",
				MetricName: "rpc.client.call.duration_count",
				Timestamp:  base,
				HasValue:   true,
				Value:      12,
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/label/__name__/values?tenant=default&start="+url.QueryEscape(base.Add(-5*time.Minute).Format(time.RFC3339))+"&end="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()

	svc.handlePrometheusLabelValues(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := []string{"rpc_client_call_duration_count"}
	if len(resp.Data) != len(want) || resp.Data[0] != want[0] {
		t.Fatalf("data = %v, want %v", resp.Data, want)
	}
}

func TestHandlePrometheusQueryMatchesNormalizedDottedMetricNames(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{
			{
				Tenant:     "default",
				Service:    "doctor",
				MetricName: "rpc.client.call.duration_count",
				Timestamp:  base,
				Attributes: map[string]string{"rpc.system": "grpc"},
				HasValue:   true,
				Value:      12,
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?tenant=default&query=rpc_client_call_duration_count&time="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()

	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := backend.lastMetricQuery.MetricName; got != "rpc.client.call.duration_count" {
		t.Fatalf("metric pushdown = %q, want rpc.client.call.duration_count", got)
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1", len(resp.Data.Result))
	}
	if got := resp.Data.Result[0].Metric["__name__"]; got != "rpc_client_call_duration_count" {
		t.Fatalf("metric name = %q, want rpc_client_call_duration_count", got)
	}
}

func TestHandlePrometheusQueryAcceptsFormEncodedPost(t *testing.T) {
	t.Helper()

	backend := &fakeLogEngineBackend{}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	body := url.Values{
		"query": []string{"1+1"},
		"time":  []string{"2026-04-30T10:00:00Z"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query?tenant=default", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []any  `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if resp.Data.ResultType != "scalar" {
		t.Fatalf("resultType = %q, want scalar", resp.Data.ResultType)
	}
}

func TestHandlePrometheusQueryRangeAcceptsFormEncodedPost(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{
			{
				Tenant:     "default",
				Service:    "api",
				MetricName: "http_requests_total",
				Timestamp:  base.Add(-1 * time.Minute),
				Attributes: map[string]string{"instance": "pod-a"},
				HasValue:   true,
				Value:      41,
			},
			{
				Tenant:     "default",
				Service:    "api",
				MetricName: "http_requests_total",
				Timestamp:  base,
				Attributes: map[string]string{"instance": "pod-a"},
				HasValue:   true,
				Value:      42,
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	body := url.Values{
		"query": []string{"http_requests_total"},
		"start": []string{base.Add(-2 * time.Minute).Format(time.RFC3339)},
		"end":   []string{base.Format(time.RFC3339)},
		"step":  []string{"30s"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query_range?tenant=default", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	svc.handlePrometheusQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if resp.Data.ResultType != "matrix" {
		t.Fatalf("resultType = %q, want matrix", resp.Data.ResultType)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1", len(resp.Data.Result))
	}
	if backend.lastMetricQuery.MetricName != "http_requests_total" {
		t.Fatalf("metric pushdown = %q, want http_requests_total", backend.lastMetricQuery.MetricName)
	}
}

func TestHandlePrometheusQueryRangeAllowsRawGuardianCumulativeTotals(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{
			Tenant:     "default",
			MetricName: "guardian_pusher_task_executions",
			Timestamp:  base.Add(-time.Minute),
			HasValue:   true,
			Value:      1,
			Attributes: map[string]string{"guardian_operation": "APPLY"},
		},
		{
			Tenant:     "default",
			MetricName: "guardian_pusher_task_executions",
			Timestamp:  base,
			HasValue:   true,
			Value:      2,
			Attributes: map[string]string{"guardian_operation": "APPLY"},
		},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query_range?tenant=default&query=guardian_pusher_task_executions&start="+url.QueryEscape(base.Add(-12*time.Hour).Format(time.RFC3339))+"&end="+url.QueryEscape(base.Format(time.RFC3339))+"&step=15s",
		nil,
	)
	rec := httptest.NewRecorder()

	svc.handlePrometheusQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if backend.lastMetricQuery.MetricName != "guardian_pusher_task_executions" {
		t.Fatalf("metric backend was queried for %q, want guardian_pusher_task_executions", backend.lastMetricQuery.MetricName)
	}
}

func TestHandlePrometheusQueryRangeAllowsAggregatedGuardianCumulativeTotals(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query_range?tenant=default&query=sum%20by%20(guardian_operation)%20(guardian_pusher_task_executions)&start="+url.QueryEscape(base.Add(-12*time.Hour).Format(time.RFC3339))+"&end="+url.QueryEscape(base.Format(time.RFC3339))+"&step=15s",
		nil,
	)
	rec := httptest.NewRecorder()

	svc.handlePrometheusQueryRange(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("aggregated range query was rejected: %s", rec.Body.String())
	}
}

func TestHandlePrometheusQueryRangePreservesMultipleLabels(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{
			{
				Tenant:     "default",
				Service:    "doctor",
				MetricName: "doctor_component_up",
				Timestamp:  base.Add(-1 * time.Minute),
				Attributes: map[string]string{"guardian_component": "doctor-ingest"},
				HasValue:   true,
				Value:      1,
			},
			{
				Tenant:     "default",
				Service:    "doctor",
				MetricName: "doctor_component_up",
				Timestamp:  base,
				Attributes: map[string]string{"guardian_component": "doctor-ingest"},
				HasValue:   true,
				Value:      1,
			},
			{
				Tenant:     "default",
				Service:    "doctor",
				MetricName: "doctor_component_up",
				Timestamp:  base.Add(-1 * time.Minute),
				Attributes: map[string]string{"guardian_component": "doctor-query"},
				HasValue:   true,
				Value:      1,
			},
			{
				Tenant:     "default",
				Service:    "doctor",
				MetricName: "doctor_component_up",
				Timestamp:  base,
				Attributes: map[string]string{"guardian_component": "doctor-query"},
				HasValue:   true,
				Value:      1,
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query_range?tenant=default&query=sum%20by%20(service%2Cguardian_component)%20(doctor_component_up)&start="+url.QueryEscape(base.Add(-2*time.Minute).Format(time.RFC3339))+"&end="+url.QueryEscape(base.Format(time.RFC3339))+"&step=30s",
		nil,
	)
	rec := httptest.NewRecorder()

	svc.handlePrometheusQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if resp.Data.ResultType != "matrix" {
		t.Fatalf("resultType = %q, want matrix", resp.Data.ResultType)
	}
	if len(resp.Data.Result) != 2 {
		t.Fatalf("result length = %d, want 2", len(resp.Data.Result))
	}
	for _, series := range resp.Data.Result {
		if got := series.Metric["service"]; got != "doctor" {
			t.Fatalf("service label = %q, want doctor", got)
		}
		if got := series.Metric["guardian_component"]; got == "" {
			t.Fatalf("guardian_component label missing in %#v", series.Metric)
		}
		if _, ok := series.Metric["__name__"]; ok {
			t.Fatalf("aggregated series unexpectedly retained __name__ in %#v", series.Metric)
		}
	}
	if backend.lastMetricQuery.MetricName != "doctor_component_up" {
		t.Fatalf("metric pushdown = %q, want doctor_component_up", backend.lastMetricQuery.MetricName)
	}
}

func TestHandlePrometheusQueryPushesServiceMatcher(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{{
			Tenant:     "default",
			Service:    "api",
			MetricName: "http_requests_total",
			Timestamp:  base,
			Attributes: map[string]string{"instance": "pod-a"},
			HasValue:   true,
			Value:      1,
		}},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=http_requests_total%7Bservice%3D%22api%22%7D&time="+strconv.FormatInt(base.Unix(), 10), nil)
	rec := httptest.NewRecorder()

	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if backend.lastMetricQuery.MetricName != "http_requests_total" {
		t.Fatalf("metric pushdown = %q, want http_requests_total", backend.lastMetricQuery.MetricName)
	}
	if backend.lastMetricQuery.Service != "api" {
		t.Fatalf("service pushdown = %q, want api", backend.lastMetricQuery.Service)
	}
}

func TestHandleLokiQueryRangeReturnsStreams(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		logs: []segment.LogRecord{
			{Service: "api", Timestamp: base.Add(-2 * time.Minute), SeverityText: "ERROR", Body: "failed request", TraceID: "trace-a"},
			{Service: "api", Timestamp: base.Add(-1 * time.Minute), SeverityText: "INFO", Body: "request ok", TraceID: "trace-b"},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/loki/api/v1/query_range?query=%7Bservice%3D~%22.%2B%22%7D&start="+strconv.FormatInt(base.Add(-5*time.Minute).UnixNano(), 10)+"&end="+strconv.FormatInt(base.UnixNano(), 10), nil)
	rec := httptest.NewRecorder()

	svc.handleLokiQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if resp.Data.ResultType != "streams" {
		t.Fatalf("resultType = %q, want streams", resp.Data.ResultType)
	}
	if len(resp.Data.Result) != 2 {
		t.Fatalf("result length = %d, want 2", len(resp.Data.Result))
	}
}

func TestHandleLokiQuerySupportsGrafanaHealthcheck(t *testing.T) {
	t.Helper()

	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	req := httptest.NewRequest(http.MethodGet, "/loki/api/v1/query?query=vector%281%29%2Bvector%281%29&time=4000000000", nil)
	rec := httptest.NewRecorder()

	svc.handleLokiQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if resp.Data.ResultType != "vector" {
		t.Fatalf("resultType = %q, want vector", resp.Data.ResultType)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1", len(resp.Data.Result))
	}
	if got, ok := resp.Data.Result[0].Value[0].(float64); !ok || got != 4000000000 {
		t.Fatalf("sample timestamp = %#v, want 4000000000", resp.Data.Result[0].Value[0])
	}
	if got, ok := resp.Data.Result[0].Value[1].(string); !ok || got != "2" {
		t.Fatalf("sample value = %#v, want \"2\"", resp.Data.Result[0].Value[1])
	}
}

func TestHandlePrometheusBuildInfoReturnsStampedRelease(t *testing.T) {
	t.Helper()

	oldVersion, oldCommit, oldBuildTime := buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime
	buildinfo.Version = "20260511-0802"
	buildinfo.Commit = "0123456789abcdef"
	buildinfo.BuildTime = "2026-05-11T08:02:00Z"
	t.Cleanup(func() {
		buildinfo.Version = oldVersion
		buildinfo.Commit = oldCommit
		buildinfo.BuildTime = oldBuildTime
	})

	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status/buildinfo", nil)
	rec := httptest.NewRecorder()

	svc.handlePrometheusBuildInfo(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string            `json:"status"`
		Data   map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if got := resp.Data["version"]; got != "20260511-0802" {
		t.Fatalf("version = %q, want 20260511-0802", got)
	}
	if got := resp.Data["revision"]; got != "0123456789abcdef" {
		t.Fatalf("revision = %q, want 0123456789abcdef", got)
	}
	if got := resp.Data["buildDate"]; got != "2026-05-11T08:02:00Z" {
		t.Fatalf("buildDate = %q, want 2026-05-11T08:02:00Z", got)
	}
}

func TestHandleLokiSeriesRespectsRequestedTimeRange(t *testing.T) {
	t.Helper()

	base := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		logs: []segment.LogRecord{
			{Service: "old-api", Timestamp: base.Add(-2 * time.Hour), SeverityText: "INFO", Body: "older log"},
			{Service: "new-api", Timestamp: base.Add(-5 * time.Minute), SeverityText: "INFO", Body: "recent log"},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(
		http.MethodGet,
		"/loki/api/v1/series?query=%7Bservice%3D~%22.%2B%22%7D&start="+strconv.FormatInt(base.Add(-3*time.Hour).UnixNano(), 10)+"&end="+strconv.FormatInt(base.Add(-90*time.Minute).UnixNano(), 10),
		nil,
	)
	rec := httptest.NewRecorder()

	svc.handleLokiSeries(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("series count = %d, want 1", len(resp.Data))
	}
	if got := resp.Data[0]["service"]; got != "old-api" {
		t.Fatalf("service = %q, want old-api", got)
	}
}

func TestHandleServiceDiscoveryGroupsComponentsAndHidesAssets(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute)
	backend := &fakeLogEngineBackend{
		traces: []segment.TraceRecord{
			{Service: "doctor.ingest.ingest", TraceID: "trace-1", StatusCode: "STATUS_CODE_UNSET", StartTime: base, EndTime: base, Partition: "doctor", Intent: "ingest", Asset: "ingest"},
			{Service: "doctor.ingest.ingest", TraceID: "trace-2", StatusCode: "STATUS_CODE_ERROR", StartTime: base, EndTime: base, Partition: "doctor", Intent: "ingest", Asset: "ingest"},
		},
		logs: []segment.LogRecord{
			{Service: "doctor.ingest.ingest", SeverityText: "ERROR", Timestamp: base, Partition: "doctor", Intent: "ingest", Asset: "ingest"},
			{Service: "doctor.ingest.ingest", SeverityText: "INFO", Timestamp: base, Partition: "doctor", Intent: "ingest", Asset: "ingest"},
		},
		metrics: []segment.MetricPointRecord{
			{
				Service:    "doctor.ingest.ingest",
				Tenant:     "default",
				MetricName: "http_requests_total",
				Timestamp:  base,
				Partition:  "doctor",
				Intent:     "ingest",
				Asset:      "ingest",
				Attributes: map[string]string{"guardian_asset_type": "Compute"},
			},
			{
				Service:    "agent.secrets.deepseek-api-key",
				Tenant:     "default",
				MetricName: "guardian_asset_observation_current",
				Timestamp:  base,
				Partition:  "agent",
				Intent:     "secrets",
				Asset:      "deepseek-api-key",
				Attributes: map[string]string{"guardian_asset_type": "Secret"},
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	type serviceResult struct {
		Name       string  `json:"name"`
		Kind       string  `json:"kind"`
		AssetType  string  `json:"asset_type"`
		Runtime    bool    `json:"runtime"`
		TraceCount int     `json:"trace_count"`
		ErrorRate  float64 `json:"error_rate"`
	}
	decode := func(query string) map[string]serviceResult {
		req := httptest.NewRequest(http.MethodGet, "/v1/services/discover?tenant=default&window=24h"+query, nil)
		rec := httptest.NewRecorder()
		svc.handleServiceDiscovery(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Services []serviceResult `json:"services"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]serviceResult{}
		for _, service := range resp.Services {
			out[service.Name] = service
		}
		return out
	}

	runtime := decode("")
	if _, ok := runtime["agent.secrets.deepseek-api-key"]; ok {
		t.Fatalf("asset leaked into the default catalog: %#v", runtime)
	}
	ingest, ok := runtime["doctor.ingest.ingest"]
	if !ok {
		t.Fatalf("doctor.ingest.ingest missing: %#v", runtime)
	}
	if ingest.Kind != serviceKindRuntime || !ingest.Runtime {
		t.Fatalf("ingest kind = %q runtime = %v", ingest.Kind, ingest.Runtime)
	}
	if ingest.TraceCount != 2 {
		t.Fatalf("ingest trace count = %d, want 2 unique traces", ingest.TraceCount)
	}

	withAssets := decode("&include_assets=true")
	asset, ok := withAssets["agent.secrets.deepseek-api-key"]
	if !ok {
		t.Fatalf("asset missing when include_assets=true: %#v", withAssets)
	}
	if asset.Kind != serviceKindAsset || asset.AssetType != "Secret" || asset.Runtime {
		t.Fatalf("asset classification = %#v", asset)
	}
}
