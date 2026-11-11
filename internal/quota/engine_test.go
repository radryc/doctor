package quota

import (
	"context"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

func testProvider(t *testing.T, defaults policy.Quota, partitions map[string]policy.Quota, subjects map[string]policy.Quota, known []string, enforce bool) *Provider {
	t.Helper()
	bundle := policy.Bundle{
		FormatVersion: 1,
		Quotas: &policy.QuotaBundle{
			Defaults:        defaults,
			Partitions:      partitions,
			Subjects:        subjects,
			KnownPartitions: known,
		},
	}
	pol := policy.New(bundle)
	return NewProvider(ProviderOptions{
		PolicyFunc: func() *policy.Policy { return pol },
		Enforce:    enforce,
	})
}

func fixedEngine(t *testing.T, provider *Provider, replicas int) *Engine {
	t.Helper()
	engine := NewEngine(provider, replicas, nil)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	engine.now = func() time.Time { return fixed }
	return engine
}

func TestIngestBytesRateEnforced(t *testing.T) {
	provider := testProvider(t, policy.Quota{
		Ingest: &policy.IngestQuota{
			Traces: &policy.SignalIngestQuota{BytesPerSecond: 100},
		},
	}, nil, nil, []string{"checkout"}, true)
	engine := fixedEngine(t, provider, 1)

	first := IngestBatch{Signal: "traces", Bytes: 60, Records: map[string]int64{"checkout": 1}}
	if err := engine.CheckIngest(context.Background(), first); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	second := IngestBatch{Signal: "traces", Bytes: 60, Records: map[string]int64{"checkout": 1}}
	if err := engine.CheckIngest(context.Background(), second); err == nil {
		t.Fatal("second batch should be rejected")
	}
}

func TestIngestObserveDoesNotReject(t *testing.T) {
	provider := testProvider(t, policy.Quota{
		Ingest: &policy.IngestQuota{
			Traces: &policy.SignalIngestQuota{BytesPerSecond: 10},
		},
	}, nil, nil, []string{"checkout"}, false)
	engine := fixedEngine(t, provider, 1)
	batch := IngestBatch{Signal: "traces", Bytes: 1000, Records: map[string]int64{"checkout": 1}}
	if err := engine.CheckIngest(context.Background(), batch); err != nil {
		t.Fatalf("observe mode should not reject: %v", err)
	}
}

func TestUnknownPartitionRejectedWhenKnownCatalog(t *testing.T) {
	provider := testProvider(t, policy.Quota{
		Ingest: &policy.IngestQuota{Traces: &policy.SignalIngestQuota{BytesPerSecond: 1000}},
	}, nil, nil, []string{"checkout"}, true)
	engine := fixedEngine(t, provider, 1)
	batch := IngestBatch{Signal: "traces", Bytes: 10, Records: map[string]int64{"ghost": 1}}
	if err := engine.CheckIngest(context.Background(), batch); err == nil {
		t.Fatal("unknown partition should be rejected")
	}
}

func TestBlockedPartitionAlwaysRejects(t *testing.T) {
	provider := testProvider(t, policy.Quota{}, map[string]policy.Quota{
		"checkout": {Blocked: true},
	}, nil, []string{"checkout"}, false)
	engine := fixedEngine(t, provider, 1)
	batch := IngestBatch{Signal: "traces", Bytes: 10, Records: map[string]int64{"checkout": 1}}
	if err := engine.CheckIngest(context.Background(), batch); err == nil {
		t.Fatal("blocked partition should reject even in observe mode")
	}
}

func TestQueryUserAndPartitionBuckets(t *testing.T) {
	provider := testProvider(t, policy.Quota{}, map[string]policy.Quota{
		"checkout": {Query: &policy.QueryQuota{RequestsPerSecond: 10}},
	}, map[string]policy.Quota{
		"user:alice": {Query: &policy.QueryQuota{RequestsPerSecond: 2}},
	}, []string{"checkout"}, true)
	engine := fixedEngine(t, provider, 1)
	id := authz.Identity{Subject: "alice"}

	for i := 0; i < 2; i++ {
		release, err := engine.AcquireQuery(context.Background(), QueryRequest{Identity: id, Partitions: []string{"checkout"}})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		release()
	}
	if _, err := engine.AcquireQuery(context.Background(), QueryRequest{Identity: id, Partitions: []string{"checkout"}}); err == nil {
		t.Fatal("third request should hit the user bucket")
	}
}

func TestQueryConcurrencyReleased(t *testing.T) {
	provider := testProvider(t, policy.Quota{}, map[string]policy.Quota{
		"checkout": {Query: &policy.QueryQuota{MaxConcurrent: 1}},
	}, nil, []string{"checkout"}, true)
	engine := fixedEngine(t, provider, 1)
	release, err := engine.AcquireQuery(context.Background(), QueryRequest{Partitions: []string{"checkout"}})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := engine.AcquireQuery(context.Background(), QueryRequest{Partitions: []string{"checkout"}}); err == nil {
		t.Fatal("second concurrent request should be rejected")
	}
	release()
	if release2, err := engine.AcquireQuery(context.Background(), QueryRequest{Partitions: []string{"checkout"}}); err != nil {
		t.Fatalf("acquire after release: %v", err)
	} else {
		release2()
	}
}

func TestMetricCardinalityCap(t *testing.T) {
	provider := testProvider(t, policy.Quota{
		Ingest: &policy.IngestQuota{
			Metrics: &policy.MetricsIngestQuota{MaxActiveSeries: 2},
		},
	}, nil, nil, []string{"checkout"}, true)
	engine := fixedEngine(t, provider, 1)
	batch := func(label string) IngestBatch {
		rec := segment.MetricPointRecord{
			Partition:  "checkout",
			MetricName: "requests_total",
			Attributes: map[string]string{"route": label},
		}
		return IngestBatch{
			Signal:  "metrics",
			Bytes:   10,
			Records: map[string]int64{"checkout": 1},
			Metrics: []segment.MetricPointRecord{rec},
		}
	}
	if err := engine.CheckIngest(context.Background(), batch("a")); err != nil {
		t.Fatalf("first series: %v", err)
	}
	if err := engine.CheckIngest(context.Background(), batch("b")); err != nil {
		t.Fatalf("second series: %v", err)
	}
	if err := engine.CheckIngest(context.Background(), batch("c")); err == nil {
		t.Fatal("third series should exceed the cap")
	}
}

func TestReplicaDivisionIsSafe(t *testing.T) {
	provider := testProvider(t, policy.Quota{
		Ingest: &policy.IngestQuota{Traces: &policy.SignalIngestQuota{BytesPerSecond: 100}},
	}, nil, nil, []string{"checkout"}, true)
	engine := fixedEngine(t, provider, 2)
	batch := IngestBatch{Signal: "traces", Bytes: 60, Records: map[string]int64{"checkout": 1}}
	if err := engine.CheckIngest(context.Background(), batch); err == nil {
		t.Fatal("per-replica budget is 50 bytes, so 60 must be rejected")
	}
}

func TestStorageQuotaRejectsOverWindow(t *testing.T) {
	provider := testProvider(t, policy.Quota{
		Storage: &policy.StorageQuota{MaxBytes: 100},
	}, nil, nil, []string{"checkout"}, true)
	engine := fixedEngine(t, provider, 1)
	first := IngestBatch{Signal: "traces", Bytes: 60, Records: map[string]int64{"checkout": 1}}
	if err := engine.CheckIngest(context.Background(), first); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	second := IngestBatch{Signal: "traces", Bytes: 60, Records: map[string]int64{"checkout": 1}}
	if err := engine.CheckIngest(context.Background(), second); err == nil {
		t.Fatal("second batch should exceed the storage window")
	}
}
