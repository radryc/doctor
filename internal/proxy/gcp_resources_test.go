package proxy

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	gcpstore "github.com/radryc/doctor/internal/store/gcp"
)

func TestGCPMonitoredResourceTypes(t *testing.T) {
	err := &gcpstore.HTTPError{
		StatusCode: 400,
		Body: `{"status":"error","errorType":"bad_data","error":"invalid parameter \"query\": substitute queries failed: ` +
			`convert vector selector failed: must specify a label matcher on the 'monitored_resource' label because ` +
			`multiple monitored resource types [cloud_run_revision cloud_run_worker_pool cloud_run_job] are possible ` +
			`for metric type \"run.googleapis.com/container/cpu/usage\": invalid argument"}`,
	}
	got := GCPMonitoredResourceTypes(err)
	want := []string{"cloud_run_revision", "cloud_run_worker_pool", "cloud_run_job"}
	if len(got) != len(want) {
		t.Fatalf("resource types = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resource types = %#v, want %#v", got, want)
		}
	}
	if GCPMonitoredResourceTypes(nil) != nil {
		t.Fatal("nil error should produce no resource types")
	}
	if GCPMonitoredResourceTypes(&gcpstore.HTTPError{StatusCode: 500, Body: "boom"}) != nil {
		t.Fatal("unrelated errors should produce no resource types")
	}
}

func TestWithGCPMonitoredResource(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{
			query: `{__name__="run.googleapis.com/container/cpu/usage"}`,
			want:  `{monitored_resource="cloud_run_revision", __name__="run.googleapis.com/container/cpu/usage"}`,
		},
		{
			query: `{}`,
			want:  `{monitored_resource="cloud_run_revision"}`,
		},
		{
			query: `sum(rate({__name__="run.googleapis.com/container/cpu/usage"}[5m]))`,
			want:  `sum(rate({monitored_resource="cloud_run_revision", __name__="run.googleapis.com/container/cpu/usage"}[5m]))`,
		},
		{
			query: `run_metric`,
			want:  `run_metric{monitored_resource="cloud_run_revision"}`,
		},
		{
			query: `{__name__="x", monitored_resource="already"}`,
			want:  `{__name__="x", monitored_resource="already"}`,
		},
		{
			query: `sum(rate(metric[5m]))`,
			want:  `sum(rate(metric[5m]))`,
		},
	}
	for _, tc := range cases {
		if got := WithGCPMonitoredResource(tc.query, "cloud_run_revision"); got != tc.want {
			t.Fatalf("WithGCPMonitoredResource(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

type fakeGCPResourceClient struct {
	GCPClient
	mu        sync.Mutex
	callCount int
	queries   []string
}

func (f *fakeGCPResourceClient) Enabled() bool { return true }

func (f *fakeGCPResourceClient) PrometheusQueryRange(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string]any, error) {
	f.mu.Lock()
	f.callCount++
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	if !strings.Contains(query, "monitored_resource") {
		return nil, &gcpstore.HTTPError{
			StatusCode: 400,
			Body: `{"status":"error","error":"must specify a label matcher on the 'monitored_resource' label because ` +
				`multiple monitored resource types [cloud_run_revision cloud_run_worker_pool cloud_run_job] are possible"}`,
		}
	}
	return map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "matrix",
			"result": []any{
				map[string]any{
					"metric": map[string]any{"monitored_resource": query},
					"values": []any{[]any{float64(1), "1"}},
				},
			},
		},
	}, nil
}

func TestGCPUpstreamPromRangeRetriesPerMonitoredResource(t *testing.T) {
	client := &fakeGCPResourceClient{}
	upstream := &GCPUpstream{Client: client}
	data, err := upstream.PromRange(
		context.Background(),
		"default",
		`{__name__="run.googleapis.com/container/cpu/usage"}`,
		time.Unix(100, 0),
		time.Unix(200, 0),
		time.Minute,
	)
	if err != nil {
		t.Fatalf("PromRange: %v", err)
	}
	if client.callCount != 4 {
		t.Fatalf("backend calls = %d, want 4 (initial + 3 resource types)", client.callCount)
	}
	result, _ := data["result"].([]any)
	if len(result) != 3 {
		t.Fatalf("merged result length = %d, want 3 (queries=%v)", len(result), client.queries)
	}
}

func TestAnnotateGCPMetricDataAddsCanonicalPartition(t *testing.T) {
	data := map[string]any{
		"resultType": "matrix",
		"result": []any{
			map[string]any{
				"metric": map[string]any{
					"__name__":     "run.googleapis.com/request_count",
					"location":     "europe-west1",
					"project_id":   "strata-gcp-dev",
					"service_name": "guardian-diva-shared-gcp-app",
				},
			},
		},
	}
	annotated := annotateGCPMetricData(data)
	result := annotated["result"].([]any)
	metric := result[0].(map[string]any)["metric"].(map[string]any)
	if got := metric["gcp_partition"]; got != "europe-west1" {
		t.Fatalf("gcp_partition = %v, want europe-west1", got)
	}
	if got := metric["gcp_region"]; got != "europe-west1" {
		t.Fatalf("gcp_region = %v, want europe-west1", got)
	}
	if got := metric["gcp_project"]; got != "strata-gcp-dev" {
		t.Fatalf("gcp_project = %v, want strata-gcp-dev", got)
	}
	if got := metric["guardian_partition"]; got != "diva-shared" {
		t.Fatalf("guardian_partition = %v, want diva-shared", got)
	}
	if got := metric["source"]; got != SourceGCP {
		t.Fatalf("source = %v, want gcp", got)
	}
}

func TestGCPPartitionFromName(t *testing.T) {
	cases := map[string]string{
		"guardian-diva-shared-gcp-app":           "diva-shared",
		"guardian-diva-shared-gcp-app-00002-876": "diva-shared",
		"guardian-divb-shared-gcp-app-00002-zj4": "divb-shared",
		"//run.googleapis.com/projects/strata-gcp-dev/locations/europe-west1/revisions/guardian-diva-shared-gcp-app-00002-876": "diva-shared",
		"/health": "",
	}
	for input, want := range cases {
		if got := gcpPartitionFromName(input); got != want {
			t.Fatalf("gcpPartitionFromName(%q) = %q, want %q", input, got, want)
		}
	}
}
