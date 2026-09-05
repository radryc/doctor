package privacy

import (
	"strings"
	"testing"
)

// TestRedactBearerToken ensures bearer tokens are fully redacted.
func TestRedactBearerToken(t *testing.T) {
	engine, _ := New("test-key")
	got := engine.SanitizeText("Bearer eyJhbGciOiJIUzI1NiJ9.dGVzdA.abcdef1234")
	if got != "[REDACTED]" {
		t.Fatalf("expected [REDACTED], got %q", got)
	}
}

// TestRedactJWT ensures JWT-looking values are redacted even without Bearer prefix.
func TestRedactJWT(t *testing.T) {
	engine, _ := New("test-key")
	got := engine.SanitizeText("eyJhbGciOiJIUzI1NiJ9.eyJ0ZXN0IjoiMSJ9.abcdefgh12345678")
	if got != "[REDACTED]" {
		t.Fatalf("expected [REDACTED], got %q", got)
	}
}

// TestRedactLongSecretKeys ensures secret-looking strings are redacted.
func TestRedactLongSecretKeys(t *testing.T) {
	engine, _ := New("test-key")
	got := engine.SanitizeText("my key is sk_live_abc123def456ghi789")
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("expected redacted secret key, got %q", got)
	}
}

// TestTokenizeEmails ensures emails in free text are tokenized.
func TestTokenizeEmails(t *testing.T) {
	engine, _ := New("test-key")
	got := engine.SanitizeText("contact admin@corp.io for help")
	if strings.Contains(got, "admin@corp.io") {
		t.Fatalf("email not tokenized: %q", got)
	}
	if !strings.Contains(got, "tok_") {
		t.Fatalf("expected tokenized email, got %q", got)
	}
}

// TestTokenizeIPv4 ensures IPv4 addresses are tokenized.
func TestTokenizeIPv4(t *testing.T) {
	engine, _ := New("test-key")
	got := engine.SanitizeText("request from 192.168.1.1 accepted")
	if strings.Contains(got, "192.168.1.1") {
		t.Fatalf("IP not tokenized: %q", got)
	}
	if !strings.Contains(got, "tok_") {
		t.Fatalf("expected tokenized IP, got %q", got)
	}
}

// TestTokenizeDeterministic ensures the same value always produces the same token.
func TestTokenizeDeterministic(t *testing.T) {
	engine, _ := New("hmac-key")
	t1 := engine.Tokenize("user@example.com")
	t2 := engine.Tokenize("user@example.com")
	if t1 != t2 {
		t.Fatalf("tokenize not deterministic: %q != %q", t1, t2)
	}
}

// TestTokenizeDifferentKeys ensures different HMAC keys produce different tokens.
func TestTokenizeDifferentKeys(t *testing.T) {
	e1, _ := New("key-1")
	e2, _ := New("key-2")
	t1 := e1.Tokenize("same-input")
	t2 := e2.Tokenize("same-input")
	if t1 == t2 {
		t.Fatalf("different keys should produce different tokens")
	}
}

// TestSanitizeEmptyString ensures empty strings pass through unchanged.
func TestSanitizeEmptyString(t *testing.T) {
	engine, _ := New("test-key")
	got := engine.SanitizeText("")
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

// TestSanitizeNilAttributes ensures nil map returns nil.
func TestSanitizeNilAttributes(t *testing.T) {
	engine, _ := New("test-key")
	got := engine.SanitizeAttributes(nil)
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// TestSanitizeUnicodeText ensures normal unicode text passes through.
func TestSanitizeUnicodeText(t *testing.T) {
	engine, _ := New("test-key")
	input := "café résumé über 日本語"
	got := engine.SanitizeText(input)
	if got != input {
		t.Fatalf("expected %q, got %q", input, got)
	}
}

// TestSanitizePasswordKey ensures all sensitive key patterns are redacted.
func TestSanitizePasswordKey(t *testing.T) {
	engine, _ := New("test-key")
	keys := []string{"password", "PASSWD", "Secret", "apiKey", "api_key", "credential"}
	for _, k := range keys {
		attrs := engine.SanitizeAttributes(map[string]string{k: "some-value"})
		if attrs[k] != "[REDACTED]" {
			t.Errorf("sensitive key %q not redacted: got %q", k, attrs[k])
		}
	}
}

// TestSanitizeTokenizableKeys ensures user identity keys are tokenized, not redacted.
func TestSanitizeTokenizableKeys(t *testing.T) {
	engine, _ := New("test-key")
	keys := []string{"email", "user_id", "userId", "account_id", "client_id", "ip", "remote_addr"}
	for _, k := range keys {
		attrs := engine.SanitizeAttributes(map[string]string{k: "test-value"})
		if !strings.HasPrefix(attrs[k], "tok_") {
			t.Errorf("tokenizable key %q expected tok_ prefix, got %q", k, attrs[k])
		}
	}
}

// TestNewEngineRejectsEmptyKey ensures the engine cannot be created with empty key.
func TestNewEngineRejectsEmptyKey(t *testing.T) {
	_, err := New("")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
	_, err = New("   ")
	if err == nil {
		t.Fatal("expected error for whitespace-only key")
	}
}
