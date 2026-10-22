package query

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// TestCountByNameFastPathUsesSeriesIndex pins the presence-aggregation fast
// path: `count by (__name__)` must be answered from series metadata, not from
// a sample scan, and must match the counts implied by the stored points.
func TestCountByNameFastPathUsesSeriesIndex(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		// guardian_requests_total: two series (instance=a from base, instance=b later)
		{MetricName: "guardian_requests_total", Service: "guardian", HasValue: true, Value: 1, Timestamp: base, Attributes: map[string]string{"instance": "a"}},
		{MetricName: "guardian_requests_total", Service: "guardian", HasValue: true, Value: 2, Timestamp: base.Add(30 * time.Second), Attributes: map[string]string{"instance": "a"}},
		{MetricName: "guardian_requests_total", Service: "guardian", HasValue: true, Value: 3, Timestamp: base.Add(60 * time.Second), Attributes: map[string]string{"instance": "b"}},
		// worker_requests_total: one series at base only
		{MetricName: "worker_requests_total", Service: "worker", HasValue: true, Value: 1, Timestamp: base, Attributes: map[string]string{"instance": "w"}},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `count by (__name__) ({__name__=~".+"})`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(2*time.Minute).Format(time.RFC3339Nano))
	params.Set("step", "60s")

	rec := doRequest(t, svc.handlePrometheusQueryRange, "/api/v1/query_range?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if response.Status != "success" || response.Data.ResultType != "matrix" {
		t.Fatalf("response = %s", rec.Body.String())
	}

	counts := map[string][]string{}
	for _, series := range response.Data.Result {
		name := series.Metric["__name__"]
		for _, value := range series.Values {
			counts[name] = append(counts[name], value[1].(string))
		}
	}
	// Steps land at base-60s, base, base+60s, base+120s.
	// guardian: instance a covers [base, base+30s], instance b covers base+60s.
	// worker: the single series covers base only.
	want := map[string][]string{
		"guardian_requests_total": {"0", "1", "1", "0"},
		"worker_requests_total":   {"0", "1", "0", "0"},
	}
	for name, expected := range want {
		got := counts[name]
		if len(got) != len(expected) {
			t.Fatalf("%s steps = %v, want %v", name, got, expected)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("%s steps = %v, want %v", name, got, expected)
			}
		}
	}
}

func TestCountByNameFastPathInstant(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "up", Service: "a", HasValue: true, Value: 1, Timestamp: base},
		{MetricName: "up", Service: "a", HasValue: true, Value: 1, Timestamp: base, Attributes: map[string]string{"instance": "b"}},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `count by (__name__) (up)`)
	params.Set("time", base.Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handlePrometheusQuery, "/api/v1/query?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Data.ResultType != "vector" || len(response.Data.Result) != 1 {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if got := response.Data.Result[0].Value[1]; got != "2" {
		t.Fatalf("count = %v, want 2", got)
	}
}

// The Service RED "Active Series" panel uses a bare count(...) over all metric
// names. It must be answered from the series index like count by (__name__),
// otherwise the broad selector scans every sample and times out.
func TestBareCountFastPathInstantUsesSeriesIndex(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "up", Service: "a", HasValue: true, Value: 1, Timestamp: base},
		{MetricName: "up", Service: "b", HasValue: true, Value: 1, Timestamp: base},
		{MetricName: "worker_requests_total", Service: "c", HasValue: true, Value: 1, Timestamp: base},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `count({__name__=~".+"})`)
	params.Set("time", base.Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handlePrometheusQuery, "/api/v1/query?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Data.ResultType != "vector" || len(response.Data.Result) != 1 {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if got := response.Data.Result[0].Value[1]; got != "3" {
		t.Fatalf("count = %v, want 3", got)
	}
}

func TestBareCountFastPathRangeUsesSeriesIndex(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "up", Service: "a", HasValue: true, Value: 1, Timestamp: base},
		{MetricName: "up", Service: "b", HasValue: true, Value: 1, Timestamp: base},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `count({__name__=~".+"})`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))
	params.Set("step", "60s")

	rec := doRequest(t, svc.handlePrometheusQueryRange, "/api/v1/query_range?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Data.ResultType != "matrix" || len(response.Data.Result) != 1 {
		t.Fatalf("response = %s", rec.Body.String())
	}
	want := []string{"0", "2", "0"}
	values := response.Data.Result[0].Values
	if len(values) != len(want) {
		t.Fatalf("values = %v, want %v", values, want)
	}
	for i, expected := range want {
		if got := values[i][1]; got != expected {
			t.Fatalf("values = %v, want %v", values, want)
		}
	}
}

// TestCountByNameFastPathRespectsLabelMatchers ensures selector filters still
// apply when answering from the series index.
func TestCountByNameFastPathRespectsLabelMatchers(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "http_requests_total", Service: "api", HasValue: true, Value: 1, Timestamp: base, Attributes: map[string]string{"status_code": "200"}},
		{MetricName: "http_requests_total", Service: "api", HasValue: true, Value: 1, Timestamp: base, Attributes: map[string]string{"status_code": "500"}},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `count by (__name__) ({status_code="500"})`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Format(time.RFC3339Nano))
	params.Set("step", "60s")

	rec := doRequest(t, svc.handlePrometheusQueryRange, "/api/v1/query_range?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data struct {
			Result []struct {
				Values [][]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Data.Result) != 1 {
		t.Fatalf("series = %d, want 1 (body=%s)", len(response.Data.Result), rec.Body.String())
	}
	values := response.Data.Result[0].Values
	if got := values[len(values)-1][1]; got != "1" {
		t.Fatalf("count = %v, want 1", got)
	}
}
