package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/radryc/doctor/internal/segment"
)

// floodBackend yields a configurable number of synthetic samples for one metric
// and answers metric-name discovery for it.
type floodBackend struct {
	name    string
	samples int
	ts      time.Time
	queried []string
}

func (f *floodBackend) QueryLogs(context.Context, string, string, string, time.Time, time.Time, int) ([]segment.LogRecord, error) {
	return nil, nil
}

func (f *floodBackend) QueryMetrics(context.Context, string, segment.MetricQuery, time.Time, time.Time) ([]segment.MetricPointRecord, error) {
	return nil, nil
}

func (f *floodBackend) StreamMetricSeries(context.Context, string, segment.MetricQuery, time.Time, time.Time, func(segment.MetricSeriesMeta) error) error {
	return nil
}

func (f *floodBackend) StreamMetrics(_ context.Context, _ string, query segment.MetricQuery, _, _ time.Time, yield func(segment.MetricPointRecord) error) error {
	for _, matcher := range query.LabelMatchers {
		if matcher.Name == doctorMetricDiscoveryMatcherName && matcher.Value == doctorMetricDiscoveryModeNames {
			return yield(segment.MetricPointRecord{MetricName: f.name, Timestamp: f.ts})
		}
	}
	if query.MetricName != "" {
		f.queried = append(f.queried, query.MetricName)
	}
	for i := 0; i < f.samples; i++ {
		if err := yield(segment.MetricPointRecord{MetricName: f.name, Timestamp: f.ts.Add(time.Duration(i) * time.Millisecond), HasValue: true, Value: float64(i)}); err != nil {
			return err
		}
	}
	return nil
}

func (f *floodBackend) QueryTraces(context.Context, string, string, string, time.Time, time.Time, int) ([]segment.TraceRecord, error) {
	return nil, nil
}

func TestPrometheusQueryRejectsOverlyBroadMetricQuery(t *testing.T) {
	// Lower the budget so the test does not materialise millions of points.
	previous := maxQueryLoadedSamples
	maxQueryLoadedSamples = 100
	defer func() { maxQueryLoadedSamples = previous }()

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	backend := &floodBackend{name: "flood_metric", samples: int(maxQueryLoadedSamples) + 1, ts: base}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=flood_metric&time="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestPrometheusQueryWithinBudgetSucceeds(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	backend := &floodBackend{name: "small_metric", samples: 10, ts: base}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=small_metric&time="+url.QueryEscape(base.Format(time.RFC3339)), nil)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Result []map[string]any `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result = %#v, want 1 series", resp.Data.Result)
	}
}

// A regex __name__ matcher must expand to only the matching stored metric names
// instead of scanning every metric.
func TestPrometheusRegexNameQueryScopesStorageByMetric(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	backend := &floodBackend{name: "guardian_results_executions", samples: 3, ts: base}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query?query="+url.QueryEscape(`{__name__=~"guardian.*"}`)+"&time="+url.QueryEscape(base.Format(time.RFC3339)),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	for _, queried := range backend.queried {
		if queried != backend.name {
			t.Fatalf("queried unexpected metric %q", queried)
		}
	}
	if len(backend.queried) == 0 {
		t.Fatal("expected the regex to resolve and query the matching metric name")
	}
}

// Correlation identifiers must never become metric series labels: they explode
// cardinality and make every broad query expensive.
func TestMetricPointLabelsDropCorrelationIDs(t *testing.T) {
	point := segment.MetricPointRecord{
		Service:    "guardian",
		Partition:  "doctor",
		MetricName: "guardian.pusher.task.executions",
		Attributes: map[string]string{
			"guardian.task.correlation_id": "corr-123",
			"guardian.partition":           "doctor",
		},
	}
	labels := metricPointLabels(point)
	labels.Range(func(label promlabels.Label) {
		switch label.Name {
		case "guardian_task_correlation_id", "guardian.task.correlation_id":
			t.Fatalf("correlation id leaked into series labels: %v", label)
		}
	})
	if got := labels.Get("guardian_partition"); got != "doctor" {
		t.Fatalf("guardian_partition = %q, want doctor", got)
	}
}
