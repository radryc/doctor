package query

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/access"
	"github.com/radryc/monofs/pkg/authz/policy"
)

func discoveryTestGate(t *testing.T) *access.PolicyGate {
	t.Helper()

	file, err := policy.ParseFile([]byte(`
version: 1
defaults:
  partitions: allow
  logs: allow
  traces: allow
`))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	catalog := &policy.Catalog{Partitions: []policy.Partition{
		{Name: "quiet"},
		{Name: "loud"},
	}}
	bundle, _, err := policy.Compile(file, catalog, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	data, err := policy.MarshalBundle(*bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	gate := access.NewPolicyGate(access.PolicyOptions{
		File:    path,
		Enforce: true,
		Logger:  log.New(io.Discard, "", 0),
	})
	if gate == nil {
		t.Fatal("expected a policy gate")
	}
	if err := gate.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh gate: %v", err)
	}
	return gate
}

func TestDiscoveryScanPartitionsUsesCatalog(t *testing.T) {
	svc := &Service{policyGate: discoveryTestGate(t)}

	got := svc.discoveryScanPartitions(context.Background(), policy.SignalMetrics)
	want := []string{"loud", "quiet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metric scan partitions = %#v, want %#v", got, want)
	}
}

func TestDiscoveryScanPartitionsWithoutGateFallsBack(t *testing.T) {
	svc := &Service{}
	if got := svc.discoveryScanPartitions(context.Background(), policy.SignalMetrics); got != nil {
		t.Fatalf("partitions = %#v, want nil fallback", got)
	}
}

func TestIntersectPartitionsKeepsCatalogOrder(t *testing.T) {
	got := intersectPartitions(
		[]string{"agent", "doctor", "memebank", "monofs"},
		[]string{"monofs", "memebank"},
	)
	want := []string{"memebank", "monofs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("intersection = %#v, want %#v", got, want)
	}
	if got := intersectPartitions([]string{"agent"}, nil); got != nil {
		t.Fatalf("empty visibility should return nil, got %#v", got)
	}
}
