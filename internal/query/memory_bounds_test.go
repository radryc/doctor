package query

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// These tests pin the memory guards on the Grafana compatibility endpoints:
// every new code path must fail cleanly instead of growing without bound.

func TestTraceQLMetricsRejectsTooManySeries(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{}
	for i := 0; i < traceMetricsMaxSeries+50; i++ {
		backend.traces = append(backend.traces, segment.TraceRecord{
			TraceID: "trace", SpanID: "span-" + strconv.Itoa(i), ParentSpanID: "",
			Name: "op", Service: "svc", StartTime: base, EndTime: base.Add(time.Millisecond),
			Attributes: map[string]string{"operation.id": "op-" + strconv.Itoa(i)},
		})
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("q", `{} | count_over_time() by (operation.id)`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))
	params.Set("step", "15s")

	rec := doRequest(t, svc.handleTempoMetricsQueryRange, "/api/metrics/query_range?"+params.Encode())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestTraceQLMetricsRejectsTooManyBuckets(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{{
		TraceID: "trace", SpanID: "span", Service: "svc", StartTime: base, EndTime: base.Add(time.Millisecond),
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("q", `{} | rate()`)
	params.Set("start", base.Add(-72*time.Hour).Format(time.RFC3339Nano))
	params.Set("end", base.Format(time.RFC3339Nano))
	params.Set("step", "1s")

	rec := doRequest(t, svc.handleTempoMetricsQueryRange, "/api/metrics/query_range?"+params.Encode())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestLokiVolumeRangeClampsStepAndSeries(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{}
	for i := 0; i < lokiVolumeMaxSeries+50; i++ {
		backend.logs = append(backend.logs, segment.LogRecord{
			Service: "svc", Timestamp: base, SeverityText: "INFO", Body: "x",
			Attributes: map[string]string{"request.id": "req-" + strconv.Itoa(i)},
		})
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", `{service=~".+"}`)
	params.Set("start", base.Add(-24*time.Hour).Format(time.RFC3339Nano))
	params.Set("end", base.Format(time.RFC3339Nano))
	params.Set("step", "1ms")
	params.Set("targetLabels", "request.id")

	rec := doRequest(t, svc.handleLokiIndexVolumeRange, "/loki/api/v1/index/volume_range?"+params.Encode())
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
	if len(response.Data.Result) > lokiVolumeMaxSeries {
		t.Fatalf("series = %d, want <= %d", len(response.Data.Result), lokiVolumeMaxSeries)
	}
	for _, series := range response.Data.Result {
		if len(series.Values) > lokiVolumeMaxBuckets+1 {
			t.Fatalf("buckets = %d, want <= %d", len(series.Values), lokiVolumeMaxBuckets)
		}
	}
}

func TestPrometheusExemplarsCapSeries(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{}
	for i := 0; i < maxExemplarSeries+50; i++ {
		backend.metrics = append(backend.metrics, segment.MetricPointRecord{
			MetricName: "up", Service: "svc", HasValue: true, Value: 1,
			Timestamp: base, TraceID: "trace-" + strconv.Itoa(i),
			Attributes: map[string]string{"instance": "inst-" + strconv.Itoa(i)},
		})
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("query", "up")
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handlePrometheusQueryExemplars, "/api/v1/query_exemplars?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data []struct {
			Exemplars []map[string]any `json:"exemplars"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Data) > maxExemplarSeries {
		t.Fatalf("series = %d, want <= %d", len(response.Data), maxExemplarSeries)
	}
}

func TestGlobalSampleBudgetIsReleasedOnClose(t *testing.T) {
	before := globalLoadedSamples.Load()
	querier, err := doctorPromQueryable{backend: &fakeLogEngineBackend{}}.Querier(0, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("querier: %v", err)
	}
	doctorQuerier, ok := querier.(*doctorPromQuerier)
	if !ok {
		t.Fatalf("unexpected querier type %T", querier)
	}
	// Simulate a query that accounted for samples.
	doctorQuerier.loadedSamples.Add(1234)
	globalLoadedSamples.Add(1234)
	if err := doctorQuerier.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := globalLoadedSamples.Load(); got != before {
		t.Fatalf("global samples = %d, want %d (budget leaked)", got, before)
	}
	// Double close must not underflow.
	if err := doctorQuerier.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if got := globalLoadedSamples.Load(); got != before {
		t.Fatalf("global samples = %d after double close, want %d", got, before)
	}
}
