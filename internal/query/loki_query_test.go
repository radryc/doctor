package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

func lokiReleaseBackend() *fakeLogEngineBackend {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return &fakeLogEngineBackend{
		logs: []segment.LogRecord{
			{Service: "guardian", Timestamp: base.Add(-2 * time.Minute), Body: `results: release partition=doctor intent=query status=Succeeded release=2026.01.01`},
			{Service: "guardian", Timestamp: base.Add(-3 * time.Minute), Body: `results: release partition=doctor intent=query status=Failed release=2026.01.02`},
		},
	}
}

func TestLokiLogfmtLabelFiltersAreApplied(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: lokiReleaseBackend()}

	query := `{service="guardian"} |= "results: release" | logfmt | status="Succeeded"`
	req := httptest.NewRequest(
		http.MethodGet,
		"/loki/api/v1/query_range?query="+url.QueryEscape(query)+
			"&start="+strconv.FormatInt(base.Add(-time.Hour).UnixNano(), 10)+
			"&end="+strconv.FormatInt(base.UnixNano(), 10),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handleLokiQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Result []struct {
				Values [][]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lines := 0
	for _, stream := range resp.Data.Result {
		lines += len(stream.Values)
	}
	if lines != 1 {
		t.Fatalf("logfmt status filter returned %d lines, want 1 (body=%s)", lines, rec.Body.String())
	}
}

func TestLokiCountOverTimeRangeReturnsMatrix(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: lokiReleaseBackend()}

	query := `sum(count_over_time({service="guardian"} |= "results: release" | logfmt | status="Succeeded" [1h]))`
	req := httptest.NewRequest(
		http.MethodGet,
		"/loki/api/v1/query_range?query="+url.QueryEscape(query)+
			"&start="+strconv.FormatInt(base.Add(-time.Hour).UnixNano(), 10)+
			"&end="+strconv.FormatInt(base.UnixNano(), 10)+
			"&step=5m",
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handleLokiQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.ResultType != "matrix" {
		t.Fatalf("resultType = %q, want matrix", resp.Data.ResultType)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1 (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	if len(resp.Data.Result[0].Values) != 13 {
		t.Fatalf("values = %d, want 13", len(resp.Data.Result[0].Values))
	}
	// The last step window covers the Succeeded record.
	last := resp.Data.Result[0].Values[len(resp.Data.Result[0].Values)-1]
	if last[1].(string) != "1" {
		t.Fatalf("last count = %v, want 1", last[1])
	}
}

func TestLokiCountOverTimeSumByUsesParsedFields(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: lokiReleaseBackend()}

	query := `sum by (release) (count_over_time({service="guardian"} |= "results: release" | logfmt [1h]))`
	req := httptest.NewRequest(
		http.MethodGet,
		"/loki/api/v1/query_range?query="+url.QueryEscape(query)+
			"&start="+strconv.FormatInt(base.Add(-time.Hour).UnixNano(), 10)+
			"&end="+strconv.FormatInt(base.UnixNano(), 10)+
			"&step=5m",
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handleLokiQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Result) != 2 {
		t.Fatalf("series = %d, want 2 (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	releases := map[string]bool{}
	for _, series := range resp.Data.Result {
		releases[series.Metric["release"]] = true
	}
	if !releases["2026.01.01"] || !releases["2026.01.02"] {
		t.Fatalf("release groups = %#v", releases)
	}
}

func TestLokiCountOverTimeInstantReturnsVector(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: lokiReleaseBackend()}

	query := `sum(count_over_time({service="guardian"} |= "results: release" [1h]))`
	req := httptest.NewRequest(
		http.MethodGet,
		"/loki/api/v1/query?query="+url.QueryEscape(query)+"&time="+strconv.FormatInt(base.Unix(), 10),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handleLokiQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.ResultType != "vector" {
		t.Fatalf("resultType = %q, want vector", resp.Data.ResultType)
	}
	if len(resp.Data.Result) != 1 || resp.Data.Result[0].Value[1].(string) != "2" {
		t.Fatalf("instant result = %#v", resp.Data.Result)
	}
}

func TestLokiDropKeepStageArgumentsAreConsumed(t *testing.T) {
	cases := []string{
		`{service="guardian"} | logfmt | json | drop __error__`,
		`{service="guardian"} | logfmt | json | drop __error__ | trace_id="abc"`,
		`{service="guardian"} | keep level | drop __error__`,
		`sum(count_over_time({service="guardian"} | json | drop __error__ [5m]))`,
		`{service="guardian"} | drop foo, bar`,
	}
	for _, query := range cases {
		if _, err := parseLokiPlan(query); err != nil {
			t.Errorf("parseLokiPlan(%q) returned error: %v", query, err)
		}
	}
}

// Grafana's trace-to-logs link builds pipelines ending in `| drop __error__`.
// This is the shape that broke the log volume query with
// `unsupported query fragment "__error__"`.
func TestLokiGrafanaTraceToLogsPipelineParses(t *testing.T) {
	query := `{service="guardian"} | logfmt | json | drop __error__ | trace_id="7946b05c2e2e4e5a" | span_id="6605c7b08e715d6c"`
	plan, err := parseLokiPlan(query)
	if err != nil {
		t.Fatalf("parseLokiPlan(%q) returned error: %v", query, err)
	}
	if !plan.hasLogfmt || !plan.hasJSON {
		t.Fatalf("expected logfmt and json stages to be recorded, got %+v", plan)
	}
	if len(plan.labelFilters) != 2 {
		t.Fatalf("label filters = %d, want 2 (%+v)", len(plan.labelFilters), plan.labelFilters)
	}
}

// Grafana's AWS CloudWatch Log Volume panel emits a postfix grouping clause:
// sum(count_over_time(...)) by (aws_log_group). Both the prefix and postfix
// forms are valid and must resolve to the same group-by labels.
func TestLokiCountOverTimePostfixByParses(t *testing.T) {
	cases := []struct {
		query   string
		groupBy []string
	}{
		{
			query:   `sum(count_over_time({source="aws",aws_region=~"us-east-1"}[5m])) by (aws_log_group)`,
			groupBy: []string{"aws_log_group"},
		},
		{
			query:   `sum(count_over_time({source="aws"}[5m])) by (aws_log_group, aws_region)`,
			groupBy: []string{"aws_log_group", "aws_region"},
		},
		{
			query:   `count_over_time({source="aws"}[5m]) by (aws_log_group)`,
			groupBy: []string{"aws_log_group"},
		},
	}
	for _, tc := range cases {
		plan, err := parseLokiPlan(tc.query)
		if err != nil {
			t.Fatalf("parseLokiPlan(%q) returned error: %v", tc.query, err)
		}
		if plan.metric == nil {
			t.Fatalf("parseLokiPlan(%q): metric plan is nil", tc.query)
		}
		if !reflect.DeepEqual(plan.metric.groupBy, tc.groupBy) {
			t.Fatalf("parseLokiPlan(%q) groupBy = %#v, want %#v", tc.query, plan.metric.groupBy, tc.groupBy)
		}
	}
}

func TestLokiCountOverTimePostfixByGroupsSeries(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{logs: []segment.LogRecord{
		{Service: "aws", Timestamp: base.Add(-2 * time.Minute), Body: "orders", Attributes: map[string]string{"aws_log_group": "/ecs/orders", "aws_region": "us-east-1"}},
		{Service: "aws", Timestamp: base.Add(-2 * time.Minute), Body: "billing", Attributes: map[string]string{"aws_log_group": "/ecs/billing", "aws_region": "us-east-1"}},
		{Service: "aws", Timestamp: base.Add(-3 * time.Minute), Body: "orders-2", Attributes: map[string]string{"aws_log_group": "/ecs/orders", "aws_region": "us-east-1"}},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	query := `sum(count_over_time({source="aws"}[1h])) by (aws_log_group)`
	req := httptest.NewRequest(
		http.MethodGet,
		"/loki/api/v1/query_range?query="+url.QueryEscape(query)+
			"&start="+strconv.FormatInt(base.Add(-time.Hour).UnixNano(), 10)+
			"&end="+strconv.FormatInt(base.UnixNano(), 10)+
			"&step=5m",
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handleLokiQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Result) != 2 {
		t.Fatalf("series = %d, want 2 (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	groups := map[string]bool{}
	for _, series := range resp.Data.Result {
		groups[series.Metric["aws_log_group"]] = true
	}
	if !groups["/ecs/orders"] || !groups["/ecs/billing"] {
		t.Fatalf("aws_log_group series = %#v", groups)
	}
}

func TestLokiCountOverTimeRejectsTrailingGarbage(t *testing.T) {
	if _, err := parseLokiPlan(`sum(count_over_time({source="aws"}[5m])) bogus`); err == nil {
		t.Fatal("expected an error for a trailing fragment after the aggregation")
	}
}
