package access

import "testing"

func TestPolicyGateCanonicalizeDottedPartition(t *testing.T) {
	gate := &PolicyGate{}
	if got := gate.canonicalize("", map[string]string{"guardian.partition": "monofs"}); got != "monofs" {
		t.Fatalf("canonicalize(dotted) = %q, want monofs", got)
	}
	if got := gate.canonicalize("", map[string]string{"guardian_partition": "doctor"}); got != "doctor" {
		t.Fatalf("canonicalize(underscore) = %q, want doctor", got)
	}
}
