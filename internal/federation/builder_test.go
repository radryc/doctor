package federation

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildCatalogJoinsPartitionsPushersAndMeta(t *testing.T) {
	result, err := BuildCatalog(BuildInput{
		Version: "v1",
		Partitions: []PartitionInfo{
			{Name: "div-a-orders", Provider: "aws", Account: "111", Region: "us-east-1", Labels: map[string]string{"division": "A"}, LogGroup: "custom/a"},
			{Name: "div-a-billing", Provider: "aws", Account: "111", Region: "us-east-1", Labels: map[string]string{"department": "A"}},
			{Name: "div-b-inventory", Provider: "aws", Account: "222", Region: "us-east-1"},
			{Name: "monofs", Provider: "kubernetes"},
		},
		Pushers: []PusherInfo{
			{Name: "aws-111", Account: "111", Region: "us-east-1", Endpoint: "http://a", TokenEnv: "TOKEN_A", Health: Health{State: "healthy"}},
			{Name: "aws-222", Account: "222", Region: "us-east-1", Endpoint: "http://b", TokenRef: "secret/b", Health: Health{State: "healthy"}},
		},
		Meta: map[string]TargetMeta{
			"111/us-east-1": {MetricNames: []string{"Invocations"}, XrayServices: []string{"svc-a"}, FetchedAt: time.Unix(100, 0).UTC()},
			"aws-222":       {MetricNames: []string{"Errors"}, LogGroups: []string{"custom/b"}},
		},
	})
	if err != nil {
		t.Fatalf("BuildCatalog() error = %v", err)
	}
	if len(result.Snapshot.Targets) != 2 {
		t.Fatalf("targets = %d, want 2 (%#v)", len(result.Snapshot.Targets), result.Snapshot.Targets)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings = %#v, want one for monofs", result.Warnings)
	}

	targetA := result.Snapshot.Targets[0]
	if targetA.ID != "aws-111-us-east-1" || len(targetA.Partitions) != 2 {
		t.Fatalf("target A = %#v", targetA)
	}
	if targetA.Labels["division"] != "A" || targetA.Labels["department"] != "A" {
		t.Fatalf("labels = %#v", targetA.Labels)
	}
	if targetA.MetricNames[0] != "Invocations" || targetA.XrayServices[0] != "svc-a" {
		t.Fatalf("meta = %#v", targetA)
	}
	if !targetA.MetadataFetchedAt.Equal(time.Unix(100, 0).UTC()) {
		t.Fatalf("metadata fetched at = %s", targetA.MetadataFetchedAt)
	}
	if !contains(targetA.LogGroups, "custom/a") || !contains(targetA.LogGroups, "guardian/111/us-east-1/div-a-billing/app") {
		t.Fatalf("log groups = %#v", targetA.LogGroups)
	}

	targetB := result.Snapshot.Targets[1]
	if targetB.TokenRef != "secret/b" || !contains(targetB.LogGroups, "custom/b") {
		t.Fatalf("target B = %#v", targetB)
	}
}

func TestBuildCatalogSkipsMissingPusherAndPrefersHealthy(t *testing.T) {
	result, err := BuildCatalog(BuildInput{
		Partitions: []PartitionInfo{
			{Name: "div-a", Provider: "aws", Account: "111", Region: "us-east-1"},
			{Name: "div-c", Provider: "aws", Account: "333", Region: "us-east-1"},
		},
		Pushers: []PusherInfo{
			{Name: "aws-111-degraded", Account: "111", Region: "us-east-1", Endpoint: "http://degraded", Health: Health{State: "degraded"}},
			{Name: "aws-111-healthy", Account: "111", Region: "us-east-1", Endpoint: "http://healthy", Health: Health{State: "healthy"}},
		},
	})
	if err != nil {
		t.Fatalf("BuildCatalog() error = %v", err)
	}
	if len(result.Snapshot.Targets) != 1 {
		t.Fatalf("targets = %#v", result.Snapshot.Targets)
	}
	if result.Snapshot.Targets[0].Endpoint != "http://healthy" {
		t.Fatalf("endpoint = %q, want healthy pusher", result.Snapshot.Targets[0].Endpoint)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings = %#v, want missing pusher warning", result.Warnings)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestTargetMetaAcceptsServiceObjects(t *testing.T) {
	var meta TargetMeta
	payload := []byte(`{"metricNames":["Invocations"],"xrayServices":[{"name":"api","region":"us-east-1","account":"111"},{"name":"worker"},"legacy"]}`)
	if err := json.Unmarshal(payload, &meta); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	want := []string{"api", "worker", "legacy"}
	if len(meta.XrayServices) != len(want) {
		t.Fatalf("XrayServices = %#v, want %#v", meta.XrayServices, want)
	}
	for i := range want {
		if meta.XrayServices[i] != want[i] {
			t.Fatalf("XrayServices = %#v, want %#v", meta.XrayServices, want)
		}
	}
}
