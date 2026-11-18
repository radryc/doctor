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

func TestSanitizeExpandedSensitiveKeys(t *testing.T) {
	engine, err := New("test-secret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sensitive := []string{
		"pan", "card_number", "cardnumber", "cvv", "cvc",
		"ssn", "tin", "iban", "routing_number", "pin",
		"dob", "passport", "phone", "user_ssn", "card.pan",
	}
	for _, k := range sensitive {
		attrs := engine.SanitizeAttributes(map[string]string{k: "sensitive-val"})
		if attrs[k] != "[REDACTED]" {
			t.Errorf("key %q = %q, want [REDACTED]", k, attrs[k])
		}
	}

	// Ensure common telemetry keys containing short substrings are not falsely redacted.
	benign := map[string]string{
		"span_id": "abc123",
		"testing": "true",
		"mapping": "v1",
	}
	got := engine.SanitizeAttributes(benign)
	for k, v := range benign {
		if got[k] != v {
			t.Errorf("benign key %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestSanitizeEmbeddedJWT(t *testing.T) {
	engine, err := New("test-secret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	line := "auth failed token=eyJhbGciOiJIUzI1NiJ9.eyJ0ZXN0IjoiMSJ9.abcdefgh12345678 status=401"
	got := engine.SanitizeText(line)
	if got != "[REDACTED]" {
		t.Fatalf("SanitizeText(embedded JWT) = %q, want [REDACTED]", got)
	}
}

func TestSanitizeTextPANAndSSN(t *testing.T) {
	engine, err := New("test-secret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// 4111111111111111 is Luhn-valid; 4111111111111112 is Luhn-invalid.
	got := engine.SanitizeText("card 4111-1111-1111-1111 and ssn 123-45-6789 nonluhn 4111-1111-1111-1112")
	if strings.Contains(got, "4111-1111-1111-1111") {
		t.Fatalf("valid PAN was not tokenized: %q", got)
	}
	if strings.Contains(got, "123-45-6789") {
		t.Fatalf("SSN was not tokenized: %q", got)
	}
	if !strings.Contains(got, "4111-1111-1111-1112") {
		t.Fatalf("non-Luhn number should not be tokenized: %q", got)
	}
	if !strings.Contains(got, "tok_") {
		t.Fatalf("expected tok_ tokens in %q", got)
	}
}
