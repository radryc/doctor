package proxy

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRouterMetricsPartitionPushdownPerProvider(t *testing.T) {
	doctor := &fakeMetricsUpstream{name: "doctor", instant: emptyVector()}
	aws := &fakeMetricsUpstream{name: "aws", instant: emptyVector()}
	gcp := &fakeMetricsUpstream{name: "gcp", instant: emptyVector()}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
			"aws":    {Name: "aws", Kind: KindAWS, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
			"gcp":    {Name: "gcp", Kind: KindGCP, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
		},
		map[string]Upstreams{
			"doctor": {Metrics: doctor},
			"aws":    {Metrics: aws},
			"gcp":    {Metrics: gcp},
		},
	)

	// Local result is empty, so both fallback providers are queried. Federated
	// backends get their own matcher; local storage keeps the query unchanged
	// and relies on the post-filter (it mixes native and AWS/GCP-ingested
	// series).
	if _, _, err := router.MetricsInstantForPartition(context.Background(), "default", "up", time.Now(), "diva-shared"); err != nil {
		t.Fatalf("MetricsInstantForPartition: %v", err)
	}
	if got := doctor.lastQueryValue(); got != "up" {
		t.Fatalf("doctor query = %q, want the original query (post-filter handles local)", got)
	}
	if got := aws.lastQueryValue(); !strings.Contains(got, `"@aws.tag.guardian.io/partition"="diva-shared"`) {
		t.Fatalf("aws query = %q, want quoted AWS tag matcher", got)
	}
	if got := gcp.lastQueryValue(); got != "up" {
		t.Fatalf("gcp query = %q, want the original query (post-filter handles the app partition)", got)
	}
}

func TestRouterMetricsPartitionPushdownOnlySelectedProvider(t *testing.T) {
	aws := &fakeMetricsUpstream{name: "aws", instant: emptyVector()}
	gcp := &fakeMetricsUpstream{name: "gcp", instant: emptyVector()}
	router := newTestRouter(t,
		map[string]Backend{
			"aws": {Name: "aws", Kind: KindAWS, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
			"gcp": {Name: "gcp", Kind: KindGCP, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
		},
		map[string]Upstreams{
			"aws": {Metrics: aws},
			"gcp": {Metrics: gcp},
		},
	)

	if _, _, err := router.MetricsInstantForPartition(
		context.Background(),
		"default",
		`CPUUtilization{source="aws"}`,
		time.Now(),
		"diva-shared",
	); err != nil {
		t.Fatalf("MetricsInstantForPartition: %v", err)
	}
	if got := aws.lastQueryValue(); got != `CPUUtilization{"@aws.tag.guardian.io/partition"="diva-shared"}` {
		t.Fatalf("aws query = %q", got)
	}
	if gcp.callCount() != 0 {
		t.Fatalf("gcp should not be queried for an AWS-only selector")
	}

	if _, _, err := router.MetricsInstantForPartition(
		context.Background(),
		"default",
		`{__name__="run.googleapis.com/request_count", source="gcp"}`,
		time.Now(),
		"europe-west1",
	); err != nil {
		t.Fatalf("MetricsInstantForPartition: %v", err)
	}
	if got := gcp.lastQueryValue(); !strings.Contains(got, `location="europe-west1"`) {
		t.Fatalf("gcp query = %q, want location matcher", got)
	}
}

func TestRouterMetricsPartitionPushdownGCPProject(t *testing.T) {
	gcp := &fakeMetricsUpstream{name: "gcp", instant: emptyVector()}
	router := newTestRouter(t,
		map[string]Backend{
			"gcp": {Name: "gcp", Kind: KindGCP, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
		},
		map[string]Upstreams{"gcp": {Metrics: gcp}},
	)
	if _, _, err := router.MetricsInstantForPartition(
		context.Background(),
		"default",
		`run_metric{source="gcp"}`,
		time.Now(),
		"strata-gcp-dev",
	); err != nil {
		t.Fatalf("MetricsInstantForPartition: %v", err)
	}
	if got := gcp.lastQueryValue(); got != "run_metric" {
		t.Fatalf("gcp query = %q, want the original query (post-filter matches gcp_project)", got)
	}
}

func TestRouterMetricsPartitionPushdownRespectsExistingMatcher(t *testing.T) {
	aws := &fakeMetricsUpstream{name: "aws", instant: emptyVector()}
	router := newTestRouter(t,
		map[string]Backend{
			"aws": {Name: "aws", Kind: KindAWS, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
		},
		map[string]Upstreams{"aws": {Metrics: aws}},
	)
	query := `CPUUtilization{"@aws.tag.guardian.io/partition"="already"}`
	if _, _, err := router.MetricsInstantForPartition(context.Background(), "default", query, time.Now(), "diva-shared"); err != nil {
		t.Fatalf("MetricsInstantForPartition: %v", err)
	}
	if got := aws.lastQueryValue(); got != `CPUUtilization{"@aws.tag.guardian.io/partition"="already"}` {
		t.Fatalf("aws query = %q, want the existing matcher left untouched", got)
	}
}
