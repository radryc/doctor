package netguard

import "testing"

func TestPolicyBlocksSSRFTargets(t *testing.T) {
	policy := Policy{AllowPrivate: true}
	for _, raw := range []string{
		"http://127.0.0.1:3100",
		"http://[::1]:3100",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/",
		"file:///etc/passwd",
		"http://0.0.0.0:3100",
	} {
		if err := policy.Validate(raw); err == nil {
			t.Fatalf("Validate(%q) = nil, want error", raw)
		}
	}
}

func TestPolicyAllowsClusterAndLoopbackWhenConfigured(t *testing.T) {
	policy := Policy{AllowPrivate: true, AllowLoopback: true}
	for _, raw := range []string{
		"http://loki.monitoring.svc.cluster.local:3100",
		"http://10.0.0.12:3100",
		"http://127.0.0.1:3100",
		"https://metrics.example.com",
	} {
		if err := policy.Validate(raw); err != nil {
			t.Fatalf("Validate(%q) = %v", raw, err)
		}
	}
}

func TestPolicyRequiresHTTPS(t *testing.T) {
	policy := Policy{RequireHTTPS: true, AllowPrivate: true}
	if err := policy.Validate("http://loki:3100"); err == nil {
		t.Fatal("plain HTTP was accepted")
	}
	if err := policy.Validate("https://loki:3100"); err != nil {
		t.Fatalf("HTTPS rejected: %v", err)
	}
}

func TestPolicyAllowlist(t *testing.T) {
	policy := Policy{AllowedHosts: []string{"*.example.com", "loki"}}
	if err := policy.Validate("http://api.example.com"); err != nil {
		t.Fatalf("wildcard allowlist rejected: %v", err)
	}
	if err := policy.Validate("http://loki:3100"); err != nil {
		t.Fatalf("exact allowlist rejected: %v", err)
	}
	if err := policy.Validate("http://evil.example.net"); err == nil {
		t.Fatal("non-allowlisted host accepted")
	}
}
