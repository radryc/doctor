package privacy

import (
	"strings"
	"testing"
)

func TestSanitizeAttributes(t *testing.T) {
	engine, err := New("test-secret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	attrs := engine.SanitizeAttributes(map[string]string{
		"email":         "user@example.com",
		"authorization": "Bearer secret-token-value",
		"client_ip":     "10.1.2.3",
		"note":          "contact user@example.com from 10.1.2.3",
	})

	if !strings.HasPrefix(attrs["email"], "tok_") {
		t.Fatalf("expected tokenized email, got %q", attrs["email"])
	}
	if attrs["authorization"] != "[REDACTED]" {
		t.Fatalf("expected redacted authorization, got %q", attrs["authorization"])
	}
	if !strings.HasPrefix(attrs["client_ip"], "tok_") {
		t.Fatalf("expected tokenized ip, got %q", attrs["client_ip"])
	}
	if strings.Contains(attrs["note"], "user@example.com") || strings.Contains(attrs["note"], "10.1.2.3") {
		t.Fatalf("expected note to be sanitized, got %q", attrs["note"])
	}
}
