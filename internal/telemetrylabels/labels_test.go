package telemetrylabels

import "testing"

func TestPartitionValueProviderOrder(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]string
		want  string
	}{
		{
			name:  "doctor",
			attrs: map[string]string{GuardianPartition: "doctor-partition"},
			want:  "doctor-partition",
		},
		{
			name: "aws",
			attrs: map[string]string{
				AWSPartitionTag:       "diva-shared",
				AWSStrataPartitionTag: "strata-shared",
			},
			want: "diva-shared",
		},
		{
			name: "gcp annotated",
			attrs: map[string]string{
				GCPPartition: "europe-west1",
				GCPProject:   "strata-gcp-dev",
			},
			want: "europe-west1",
		},
		{
			name:  "gcp project fallback",
			attrs: map[string]string{GCPProjectID: "strata-gcp-dev"},
			want:  "strata-gcp-dev",
		},
		{
			name:  "generic partition fallback",
			attrs: map[string]string{Partition: "plain"},
			want:  "plain",
		},
		{
			name:  "empty",
			attrs: map[string]string{},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PartitionValue(tc.attrs); got != tc.want {
				t.Fatalf("PartitionValue() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIntentAndAssetValue(t *testing.T) {
	attrs := map[string]string{
		AWSIntentTag: "intent-aws",
		AWSAssetTag:  "asset-aws",
	}
	if got := IntentValue(attrs); got != "intent-aws" {
		t.Fatalf("IntentValue() = %q", got)
	}
	if got := AssetValue(attrs); got != "asset-aws" {
		t.Fatalf("AssetValue() = %q", got)
	}
}

func TestMatchesPartitionAcceptsEquivalentGCPLabels(t *testing.T) {
	attrs := map[string]string{
		GCPPartition: "europe-west1",
		GCPProject:   "strata-gcp-dev",
		GCPProjectID: "strata-gcp-dev",
		GCPLocation:  "europe-west1",
	}
	for _, partition := range []string{"europe-west1", "strata-gcp-dev"} {
		if !MatchesPartition(attrs, partition) {
			t.Fatalf("MatchesPartition(%q) = false, want true", partition)
		}
	}
	if MatchesPartition(attrs, "us-central1") {
		t.Fatal("MatchesPartition(us-central1) = true, want false")
	}
	if !MatchesPartition(attrs, "") {
		t.Fatal("empty partition should match (no filter)")
	}
}
