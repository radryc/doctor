package identity

import "testing"

func TestCanonicalServiceName(t *testing.T) {
	tests := []struct {
		partition string
		intent    string
		asset     string
		want      string
	}{
		{partition: "doctor", intent: "query", asset: "query", want: "doctor.query.query"},
		{partition: "monofs", intent: "monofs-storage", asset: "node-a", want: "monofs.monofs-storage.node-a"},
		{partition: "", intent: "query", asset: "query", want: "query.query"},
		{partition: "", intent: "", asset: "", want: ""},
	}
	for _, test := range tests {
		if got := CanonicalServiceName(test.partition, test.intent, test.asset); got != test.want {
			t.Fatalf("CanonicalServiceName(%q, %q, %q) = %q, want %q", test.partition, test.intent, test.asset, got, test.want)
		}
	}
}

func TestCanonicalServiceNameTruncatesDeterministically(t *testing.T) {
	long := make([]byte, 120)
	for i := range long {
		long[i] = 'a'
	}
	partition := string(long)
	first := CanonicalServiceName(partition, partition, "asset")
	second := CanonicalServiceName(partition, partition, "asset")
	if first != second {
		t.Fatalf("canonical name is not deterministic: %q != %q", first, second)
	}
	if len(first) > MaxServiceNameLength {
		t.Fatalf("canonical name length = %d, want <= %d", len(first), MaxServiceNameLength)
	}
	if first == partition+"."+partition+".asset" {
		t.Fatalf("expected a truncated name with hash suffix")
	}
}
