package netguard

import (
	"errors"
	"net"
	"testing"
)

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
	policy := Policy{
		AllowPrivate:  true,
		AllowLoopback: true,
		LookupIP: func(host string) ([]net.IP, error) {
			switch host {
			case "loki.monitoring.svc.cluster.local":
				return []net.IP{net.ParseIP("10.0.0.12")}, nil
			case "metrics.example.com":
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			default:
				return nil, errors.New("unexpected host")
			}
		},
	}
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
	policy := Policy{
		RequireHTTPS: true,
		AllowPrivate: true,
		LookupIP: func(host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.12")}, nil
		},
	}
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

func TestPolicyDNSResolutionSSRF(t *testing.T) {
	basePolicy := func(ips []net.IP, err error) Policy {
		return Policy{
			AllowPrivate: true,
			LookupIP: func(host string) ([]net.IP, error) {
				return ips, err
			},
		}
	}

	// DNS resolving to loopback is rejected when AllowLoopback is false.
	if err := basePolicy([]net.IP{net.ParseIP("127.0.0.1")}, nil).Validate("https://rebind.example.com"); err == nil {
		t.Fatal("expected error for DNS resolving to loopback")
	}

	// DNS resolving to metadata/link-local is always rejected, even with AllowPrivate & AllowLoopback.
	pBoth := basePolicy([]net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("169.254.169.254")}, nil)
	pBoth.AllowLoopback = true
	if err := pBoth.Validate("https://rebind-meta.example.com"); err == nil {
		t.Fatal("expected error when any resolved IP is link-local")
	}

	// DNS resolving to private IP is rejected when AllowPrivate is false.
	pPublicOnly := Policy{
		RequireHTTPS: true,
		LookupIP: func(host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		},
	}
	if err := pPublicOnly.Validate("https://internal.example.com"); err == nil {
		t.Fatal("expected error for DNS resolving to private IP when AllowPrivate=false")
	}

	// DNS lookup failure fails closed.
	if err := basePolicy(nil, errors.New("dns timeout")).Validate("https://missing.example.com"); err == nil {
		t.Fatal("expected error when DNS lookup fails")
	}
}

func TestPolicyRejectsNonHTTPSchemesWhenEnabled(t *testing.T) {
	policies := []Policy{
		{RequireHTTPS: true},
		{AllowLoopback: true},
		{AllowPrivate: true},
		{AllowedHosts: []string{"example.com"}},
	}
	for _, p := range policies {
		for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/file", "gopher://example.com:70/1"} {
			if err := p.Validate(raw); err == nil {
				t.Fatalf("policy %+v accepted non-HTTP scheme %q", p, raw)
			}
		}
	}
}

func TestZeroValuePolicyRequiresHTTPS(t *testing.T) {
	policy := Policy{
		LookupIP: func(string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	if err := policy.Validate("http://metrics.example.com"); err == nil {
		t.Fatal("zero-value policy accepted plain HTTP")
	}
	if err := policy.Validate("https://metrics.example.com"); err != nil {
		t.Fatalf("zero-value policy rejected HTTPS: %v", err)
	}
}

func TestZeroValuePolicyStillBlocksInternalTargets(t *testing.T) {
	policy := Policy{}
	for _, raw := range []string{
		"https://127.0.0.1:3100",
		"https://[::1]:3100",
		"https://10.0.0.12:3100",
		"https://169.254.169.254/latest/meta-data",
	} {
		if err := policy.Validate(raw); err == nil {
			t.Fatalf("zero-value policy accepted %q", raw)
		}
	}
}

func TestExplicitPrivateOptInAllowsHTTP(t *testing.T) {
	policy := Policy{
		AllowPrivate: true,
		LookupIP: func(string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.12")}, nil
		},
	}
	if err := policy.Validate("http://loki.monitoring.svc.cluster.local:3100"); err != nil {
		t.Fatalf("explicit AllowPrivate rejected HTTP backend: %v", err)
	}
}
