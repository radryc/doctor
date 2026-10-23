package federation

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func scaleTargets(count int) []Target {
	targets := make([]Target, 0, count)
	for i := 0; i < count; i++ {
		targets = append(targets, Target{
			ID:           fmt.Sprintf("aws-%012d-us-east-1", i),
			Account:      fmt.Sprintf("%012d", i),
			Region:       "us-east-1",
			Partitions:   []string{fmt.Sprintf("part-%05d", i)},
			Endpoint:     fmt.Sprintf("http://pusher-%d:19090", i),
			TokenRef:     "monofs-secret://doctor/federation/token",
			MetricNames:  []string{"Invocations", "Errors"},
			LogGroups:    []string{fmt.Sprintf("guardian/%012d/us-east-1/part-%05d/app", i, i)},
			XrayServices: []string{"shared-service"},
		})
	}
	return targets
}

func TestNewCatalogHotKeysDoNotDuplicatePostings(t *testing.T) {
	const count = 20000
	catalog, err := NewCatalog(scaleTargets(count), "scale")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	if got := len(catalog.ByRegions([]string{"us-east-1"})); got != count {
		t.Fatalf("region postings = %d, want %d", got, count)
	}
	if got := len(catalog.ByMetric("Invocations")); got != count {
		t.Fatalf("metric postings = %d, want %d", got, count)
	}
	if got := len(catalog.ByServices([]string{"shared-service"})); got != count {
		t.Fatalf("service postings = %d, want %d", got, count)
	}
}

func TestCatalogTargetsReturnsStableSortedCopy(t *testing.T) {
	catalog, err := NewCatalog([]Target{
		{ID: "aws-b", Account: "2", Region: "us-east-1", Endpoint: "http://b", Token: "b"},
		{ID: "aws-a", Account: "1", Region: "us-east-1", Endpoint: "http://a", Token: "a"},
	}, "v1")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	first := catalog.Targets()
	if len(first) != 2 || first[0].ID != "aws-a" || first[1].ID != "aws-b" {
		t.Fatalf("Targets() = %#v, want sorted [aws-a aws-b]", first)
	}
	first[0].ID = "mutated"
	second := catalog.Targets()
	if second[0].ID != "aws-a" {
		t.Fatalf("Targets() exposed internal storage: %#v", second)
	}
}

func TestManagerStatusCapsPartitionMap(t *testing.T) {
	targets := make([]Target, 0, statusMaxPartitionEntries+10)
	for i := 0; i < statusMaxPartitionEntries+10; i++ {
		targets = append(targets, Target{
			ID:         fmt.Sprintf("aws-%d", i),
			Account:    fmt.Sprintf("%012d", i),
			Region:     "us-east-1",
			Partitions: []string{fmt.Sprintf("part-%04d", i)},
			Endpoint:   fmt.Sprintf("http://pusher-%d", i),
			Token:      "token",
		})
	}
	catalog, err := NewCatalog(targets, "v1")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	status := NewManager(catalog, Options{}).Status()
	if got := status["partition_count"]; got != len(targets) {
		t.Fatalf("partition_count = %v, want %d", got, len(targets))
	}
	partitions, ok := status["partitions"].(map[string]int)
	if !ok {
		t.Fatalf("partitions type = %T", status["partitions"])
	}
	if len(partitions) != statusMaxPartitionEntries {
		t.Fatalf("partitions map length = %d, want %d", len(partitions), statusMaxPartitionEntries)
	}
	if truncated, _ := status["partitions_truncated"].(bool); !truncated {
		t.Fatal("partitions_truncated = false, want true")
	}
}

// TestNewCatalog150kBudget is opt-in because it allocates a large catalog.
// Run with DOCTOR_LOADTEST=1.
func TestNewCatalog150kBudget(t *testing.T) {
	if os.Getenv("DOCTOR_LOADTEST") != "1" {
		t.Skip("set DOCTOR_LOADTEST=1 to run the 150k catalog budget test")
	}
	start := time.Now()
	catalog, err := NewCatalog(scaleTargets(150000), "scale")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("NewCatalog(150000) = %s", elapsed)
	if elapsed > 5*time.Second {
		t.Fatalf("NewCatalog(150000) took %s, budget is 5s", elapsed)
	}
	if catalog.Len() != 150000 {
		t.Fatalf("targets = %d, want 150000", catalog.Len())
	}
}

func BenchmarkNewCatalog50k(b *testing.B) {
	benchmarkNewCatalog(b, 50000)
}

func BenchmarkNewCatalog150k(b *testing.B) {
	benchmarkNewCatalog(b, 150000)
}

func benchmarkNewCatalog(b *testing.B, count int) {
	targets := scaleTargets(count)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NewCatalog(targets, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}
