package telemetrylabels

import "testing"

func TestApplyPartitionDoctor(t *testing.T) {
	got := ApplyPartition(`{__name__="guardian_results_executions"}`, ProviderDoctor, "doctor")
	want := `{guardian_partition="doctor", __name__="guardian_results_executions"}`
	if got != want {
		t.Fatalf("ApplyPartition() = %q, want %q", got, want)
	}
	if got := ApplyPartition("guardian_results_executions", ProviderDoctor, "doctor"); got != `guardian_results_executions{guardian_partition="doctor"}` {
		t.Fatalf("bare metric = %q", got)
	}
}

func TestApplyPartitionAWSQuotesUTF8Label(t *testing.T) {
	got := ApplyPartition("CPUUtilization", ProviderAWS, "diva-shared")
	want := `CPUUtilization{"@aws.tag.guardian.io/partition"="diva-shared"}`
	if got != want {
		t.Fatalf("ApplyPartition() = %q, want %q", got, want)
	}
}

func TestApplyPartitionGCPRegionAndProject(t *testing.T) {
	got := ApplyPartition(`{__name__="run.googleapis.com/request_count"}`, ProviderGCP, "europe-west1")
	want := `{location="europe-west1", __name__="run.googleapis.com/request_count"}`
	if got != want {
		t.Fatalf("region = %q, want %q", got, want)
	}
	// Project and application partitions do not map to a single native GCP
	// label; the merged-result filter handles them.
	for _, partition := range []string{"strata-gcp-dev", "diva-shared"} {
		query := "run_metric"
		if got := ApplyPartition(query, ProviderGCP, partition); got != query {
			t.Fatalf("ApplyPartition(%q) = %q, want unchanged", partition, got)
		}
	}
}

func TestApplyPartitionDoesNotDuplicateMatcher(t *testing.T) {
	query := `CPUUtilization{"@aws.tag.guardian.io/partition"="already"}`
	if got := ApplyPartition(query, ProviderAWS, "diva-shared"); got != query {
		t.Fatalf("ApplyPartition() = %q, want unchanged", got)
	}
	if !HasLabelMatcher(query, AWSPartitionTag) {
		t.Fatal("HasLabelMatcher did not detect the quoted AWS partition label")
	}
	if HasLabelMatcher("CPUUtilization", AWSPartitionTag) {
		t.Fatal("HasLabelMatcher false positive")
	}
}

func TestApplyPartitionLeavesComplexExpressionsAlone(t *testing.T) {
	query := "sum(rate(metric[5m]))"
	if got := ApplyPartition(query, ProviderDoctor, "doctor"); got != query {
		t.Fatalf("ApplyPartition() = %q, want unchanged", got)
	}
}

func TestPartitionMatcherUnsupportedProvider(t *testing.T) {
	if _, _, ok := PartitionMatcher("prometheus", "p"); ok {
		t.Fatal("unexpected matcher for an unsupported provider")
	}
	if _, _, ok := PartitionMatcher(ProviderDoctor, ""); ok {
		t.Fatal("unexpected matcher for an empty partition")
	}
}
