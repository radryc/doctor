package query

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

func TestBroadSelectorRangeIsRejected(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &seriesIndexBackend{fakeLogEngineBackend: &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "up", Service: "guardian", Timestamp: base, HasValue: true, Value: 1},
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `{__name__=~".+"}`)
	params.Set("start", base.Add(-time.Hour).Format(time.RFC3339Nano))
	params.Set("end", base.Format(time.RFC3339Nano))
	params.Set("step", "30s")

	rec := doRequest(t, svc.handlePrometheusQueryRange, "/api/v1/query_range?"+params.Encode())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "selector matches all metric series") {
		t.Fatalf("body = %s, want broad-selector error", rec.Body.String())
	}
	if backend.sampleScans != 0 {
		t.Fatalf("broad query scanned samples %d times, want 0", backend.sampleScans)
	}
}

func TestBroadSelectorShortRangeIsAllowed(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "up", Service: "guardian", Timestamp: base, HasValue: true, Value: 1},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `{__name__=~".+"}`)
	params.Set("start", base.Add(-5*time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Format(time.RFC3339Nano))
	params.Set("step", "30s")

	rec := doRequest(t, svc.handlePrometheusQueryRange, "/api/v1/query_range?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestBroadSelectorCountFastPathStillWorks(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "up", Service: "guardian", Timestamp: base, HasValue: true, Value: 1},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `count by (__name__) ({__name__=~".+"})`)
	params.Set("start", base.Add(-time.Hour).Format(time.RFC3339Nano))
	params.Set("end", base.Format(time.RFC3339Nano))
	params.Set("step", "30s")

	rec := doRequest(t, svc.handlePrometheusQueryRange, "/api/v1/query_range?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestBroadSelectorInsideAggregationIsRejected(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &seriesIndexBackend{fakeLogEngineBackend: &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "up", Service: "guardian", Timestamp: base, HasValue: true, Value: 1},
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `sum({__name__=~".+"})`)
	params.Set("start", base.Add(-time.Hour).Format(time.RFC3339Nano))
	params.Set("end", base.Format(time.RFC3339Nano))
	params.Set("step", "30s")

	rec := doRequest(t, svc.handlePrometheusQueryRange, "/api/v1/query_range?"+params.Encode())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if backend.sampleScans != 0 {
		t.Fatalf("broad aggregate scanned samples %d times, want 0", backend.sampleScans)
	}
}
