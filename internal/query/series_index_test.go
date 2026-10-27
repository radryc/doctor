package query

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// seriesIndexBackend fails if discovery touches sample data. It is used to pin
// the /api/v1/series series-index fast path.
type seriesIndexBackend struct {
	*fakeLogEngineBackend
	sampleScans int
}

func (b *seriesIndexBackend) StreamMetrics(context.Context, string, segment.MetricQuery, time.Time, time.Time, func(segment.MetricPointRecord) error) error {
	b.sampleScans++
	return fmt.Errorf("unexpected sample scan")
}

func TestPrometheusSeriesUsesSeriesIndex(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &seriesIndexBackend{fakeLogEngineBackend: &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{MetricName: "guardian_requests_total", Service: "guardian", Timestamp: base, HasValue: true, Value: 1, Attributes: map[string]string{"instance": "a"}},
		{MetricName: "worker_requests_total", Service: "worker", Timestamp: base, HasValue: true, Value: 2, Attributes: map[string]string{"instance": "b"}},
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	params := url.Values{}
	params.Set("match[]", `{__name__=~".+"}`)
	params.Set("start", base.Add(-time.Hour).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handlePrometheusSeries, "/api/v1/series?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if backend.sampleScans != 0 {
		t.Fatalf("series discovery scanned samples %d times, want 0", backend.sampleScans)
	}
	var response struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if len(response.Data) != 2 {
		t.Fatalf("series result = %#v, want 2 series", response.Data)
	}
	for _, labels := range response.Data {
		if labels["__name__"] == "" || labels["instance"] == "" {
			t.Fatalf("series labels = %#v, want name and instance", labels)
		}
	}
}
