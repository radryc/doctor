package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/segment"
)

type fakeGCPCatalogClient struct {
	proxy.GCPClient
	names []string
	err   error
	calls int
}

func (f *fakeGCPCatalogClient) Enabled() bool { return true }

func (f *fakeGCPCatalogClient) PrometheusLabelValues(_ context.Context, label string, _ []string, _, _ time.Time) (map[string]any, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if label != "__name__" {
		return map[string]any{"status": "success", "data": []string{"gcp-label"}}, nil
	}
	return map[string]any{"status": "success", "data": f.names}, nil
}

func decodeLabelValues(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Data
}

func TestMetricCatalogSourceParsing(t *testing.T) {
	cases := []struct {
		raw    string
		doctor bool
		aws    bool
		gcp    bool
		bad    bool
	}{
		{raw: "", doctor: true, aws: true},
		{raw: "all", doctor: true, aws: true, gcp: true},
		{raw: "doctor", doctor: true},
		{raw: "gcp", gcp: true},
		{raw: "doctor,gcp", doctor: true, gcp: true},
		{raw: "aws,gcp", aws: true, gcp: true},
		{raw: "bogus", bad: true},
	}
	for _, tc := range cases {
		got, err := parseMetricCatalogSource(tc.raw)
		if tc.bad {
			if err == nil {
				t.Fatalf("parseMetricCatalogSource(%q) = %+v, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseMetricCatalogSource(%q): %v", tc.raw, err)
		}
		if got.doctor != tc.doctor || got.aws != tc.aws || got.gcp != tc.gcp {
			t.Fatalf("parseMetricCatalogSource(%q) = %+v, want doctor=%v aws=%v gcp=%v", tc.raw, got, tc.doctor, tc.aws, tc.gcp)
		}
	}
}

func TestFilterMetricCatalogRanksPrefixes(t *testing.T) {
	names := []string{"kubernetes.io/running_managed_controllers", "run.googleapis.com/request_count", "run.googleapis.com/container/cpu"}
	got := filterMetricCatalog(names, "run")
	want := []string{"run.googleapis.com/container/cpu", "run.googleapis.com/request_count", "kubernetes.io/running_managed_controllers"}
	if len(got) != len(want) {
		t.Fatalf("filtered = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("filtered = %#v, want %#v", got, want)
		}
	}
}

func TestArrangeMetricCatalogInterleavesProviders(t *testing.T) {
	priorities := map[string]int{
		"doctor_a": metricCatalogDoctorPriority,
		"doctor_b": metricCatalogDoctorPriority,
		"aws_a":    metricCatalogAWSPriority,
		"aws_b":    metricCatalogAWSPriority,
		"gcp_a":    metricCatalogGCPPriority,
		"gcp_b":    metricCatalogGCPPriority,
	}
	// Input is already filtered and ordered; grouping must preserve that order.
	names := []string{"doctor_a", "doctor_b", "aws_a", "aws_b", "gcp_a", "gcp_b"}
	got := arrangeMetricCatalog(names, priorities, 4)
	want := []string{"doctor_a", "aws_a", "gcp_a", "doctor_b"}
	if len(got) != len(want) {
		t.Fatalf("arranged = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arranged = %#v, want %#v", got, want)
		}
	}
	if got := arrangeMetricCatalog(names, priorities, 0); len(got) != len(names) {
		t.Fatalf("unlimited arranged length = %d, want %d", len(got), len(names))
	}
}

func TestPrometheusMetricNamesSearchLimitAndGCP(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		defaultTenant: "default",
		logEngine: &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{{
			MetricName: "local_metric",
			Timestamp:  base.Add(-time.Minute),
			HasValue:   true,
			Value:      1,
		}}},
		gcp: &fakeGCPCatalogClient{names: []string{
			"run.googleapis.com/request_count",
			"run.googleapis.com/container/cpu",
			"storage.googleapis.com/request_count",
		}},
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/label/__name__/values?source=all&q=run&limit=2&start="+url.QueryEscape(base.Add(-time.Hour).Format(time.RFC3339))+
			"&end="+url.QueryEscape(base.Format(time.RFC3339)),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusLabelValues(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeLabelValues(t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("values = %#v, want 2 entries", got)
	}
	if got[0] != "run.googleapis.com/container/cpu" || got[1] != "run.googleapis.com/request_count" {
		t.Fatalf("values = %#v, want the two run.googleapis.com metrics", got)
	}
	if total := rec.Header().Get("X-Doctor-Metric-Total"); total != "2" {
		t.Fatalf("X-Doctor-Metric-Total = %q, want 2", total)
	}
	if truncated := rec.Header().Get("X-Doctor-Metric-Truncated"); truncated == "true" {
		t.Fatalf("X-Doctor-Metric-Truncated = %q, want unset", truncated)
	}
}

func TestPrometheusMetricNamesGCPTruncationHeader(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		defaultTenant: "default",
		logEngine:     &fakeLogEngineBackend{},
		gcp: &fakeGCPCatalogClient{names: []string{
			"gcp_a", "gcp_b", "gcp_c",
		}},
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/label/__name__/values?source=gcp&limit=2&start="+url.QueryEscape(base.Add(-time.Hour).Format(time.RFC3339))+
			"&end="+url.QueryEscape(base.Format(time.RFC3339)),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusLabelValues(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeLabelValues(t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("values = %#v, want 2 entries", got)
	}
	if total := rec.Header().Get("X-Doctor-Metric-Total"); total != "3" {
		t.Fatalf("X-Doctor-Metric-Total = %q, want 3", total)
	}
	if truncated := rec.Header().Get("X-Doctor-Metric-Truncated"); truncated != "true" {
		t.Fatalf("X-Doctor-Metric-Truncated = %q, want true", truncated)
	}
}
