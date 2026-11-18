package privacy

import (
	"encoding/hex"
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

	if !strings.HasPrefix(attrs["email"], "tok_v2_k1_") {
		t.Fatalf("expected versioned tokenized email, got %q", attrs["email"])
	}
	if attrs["authorization"] != "[REDACTED]" {
		t.Fatalf("expected redacted authorization, got %q", attrs["authorization"])
	}
	if !strings.HasPrefix(attrs["client_ip"], "tok_v2_k1_") {
		t.Fatalf("expected versioned tokenized ip, got %q", attrs["client_ip"])
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
	if !strings.Contains(got, "tok_v2_k1_") {
		t.Fatalf("expected tok_v2_k1_ tokens in %q", got)
	}
}

func TestTokenizeVersioned128BitFormat(t *testing.T) {
	engine, err := New("test-secret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tok := engine.Tokenize("alice@example.com")
	if !strings.HasPrefix(tok, "tok_v2_k1_") {
		t.Fatalf("expected tok_v2_k1_ prefix, got %q", tok)
	}
	tagHex := strings.TrimPrefix(tok, "tok_v2_k1_")
	if len(tagHex) != 32 {
		t.Fatalf("expected 32 hex chars (128-bit tag), got %d in %q", len(tagHex), tok)
	}
	rawTag, err := hex.DecodeString(tagHex)
	if err != nil || len(rawTag) != 16 {
		t.Fatalf("expected 16 decoded bytes, got len=%d err=%v", len(rawTag), err)
	}
	if !engine.VerifyToken(tok, "alice@example.com") {
		t.Fatalf("VerifyToken() failed for freshly minted token %q", tok)
	}
}

func TestKeyRotationAndVerification(t *testing.T) {
	legacyEngine, err := New("old-secret-k1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	historicalTok := legacyEngine.Tokenize("user-123")

	rotated, err := NewWithKeys("k2", map[string]string{
		"k1": "old-secret-k1",
		"k2": "new-secret-k2",
	})
	if err != nil {
		t.Fatalf("NewWithKeys() error = %v", err)
	}

	newTok := rotated.Tokenize("user-123")
	if !strings.HasPrefix(newTok, "tok_v2_k2_") || len(strings.TrimPrefix(newTok, "tok_v2_k2_")) != 32 {
		t.Fatalf("expected tok_v2_k2_<32-hex>, got %q", newTok)
	}
	if newTok == historicalTok {
		t.Fatalf("expected rotated active key to produce a distinct token")
	}

	// Both historical (k1) and active (k2) tokens must verify.
	if !rotated.VerifyToken(historicalTok, "user-123") {
		t.Fatalf("expected rotated engine to verify historical k1 token")
	}
	if !rotated.VerifyToken(newTok, "user-123") {
		t.Fatalf("expected rotated engine to verify active k2 token")
	}

	// TokenizeWithKey mints under the requested key ID.
	k1Tok, err := rotated.TokenizeWithKey("k1", "user-123")
	if err != nil {
		t.Fatalf("TokenizeWithKey(k1) error = %v", err)
	}
	if k1Tok != historicalTok {
		t.Fatalf("TokenizeWithKey(k1) = %q, want %q", k1Tok, historicalTok)
	}
	if _, err := rotated.TokenizeWithKey("k99", "user-123"); err == nil {
		t.Fatalf("expected error for unknown key ID")
	}

	// Negative verification cases.
	badCases := []struct {
		name  string
		token string
		val   string
	}{
		{"wrong raw value", newTok, "user-456"},
		{"unknown key id", "tok_v2_k99_" + strings.Repeat("a", 32), "user-123"},
		{"legacy 64-bit token", "tok_" + strings.Repeat("a", 16), "user-123"},
		{"short tag", "tok_v2_k2_" + strings.Repeat("a", 16), "user-123"},
		{"non-hex tag", "tok_v2_k2_" + strings.Repeat("z", 32), "user-123"},
		{"missing kid", "tok_v2__" + strings.Repeat("a", 32), "user-123"},
	}
	for _, tc := range badCases {
		if rotated.VerifyToken(tc.token, tc.val) {
			t.Errorf("VerifyToken(%s) unexpectedly returned true", tc.name)
		}
	}
}

func TestNewWithKeysValidation(t *testing.T) {
	if _, err := NewWithKeys("k1", nil); err == nil {
		t.Fatal("expected error for empty keys map")
	}
	if _, err := NewWithKeys("k1", map[string]string{"k1": "  "}); err == nil {
		t.Fatal("expected error for blank secret")
	}
	if _, err := NewWithKeys("k2", map[string]string{"k1": "secret"}); err == nil {
		t.Fatal("expected error when active key ID is missing from keys")
	}
	if _, err := NewWithKeys("!!!", map[string]string{"k1": "secret"}); err == nil {
		t.Fatal("expected error for non-alphanumeric active key ID")
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Run("fallback to DOCTOR_PRIVACY_HMAC_KEY with k1", func(t *testing.T) {
		t.Setenv("DOCTOR_PRIVACY_KEY_ID", "")
		t.Setenv("DOCTOR_PRIVACY_KEYS_JSON", "")
		t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "env-secret-1")

		engine, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		tok := engine.Tokenize("val")
		if !strings.HasPrefix(tok, "tok_v2_k1_") || len(strings.TrimPrefix(tok, "tok_v2_k1_")) != 32 {
			t.Fatalf("unexpected token format %q", tok)
		}
	})

	t.Run("sanitizes DOCTOR_PRIVACY_KEY_ID", func(t *testing.T) {
		t.Setenv("DOCTOR_PRIVACY_KEY_ID", "k2_prod-1!")
		t.Setenv("DOCTOR_PRIVACY_KEYS_JSON", "")
		t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "env-secret-2")

		engine, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		tok := engine.Tokenize("val")
		if !strings.HasPrefix(tok, "tok_v2_k2prod1_") {
			t.Fatalf("expected sanitized kid tok_v2_k2prod1_, got %q", tok)
		}
	})

	t.Run("multi-key rotation via DOCTOR_PRIVACY_KEYS_JSON", func(t *testing.T) {
		t.Setenv("DOCTOR_PRIVACY_KEY_ID", "k3")
		t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "")
		t.Setenv("DOCTOR_PRIVACY_KEYS_JSON", `{"k1":"s1","k2":"s2","k3":"s3"}`)

		engine, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		tok3 := engine.Tokenize("user@example.com")
		if !strings.HasPrefix(tok3, "tok_v2_k3_") {
			t.Fatalf("expected active key k3, got %q", tok3)
		}
		tok1, err := engine.TokenizeWithKey("k1", "user@example.com")
		if err != nil {
			t.Fatalf("TokenizeWithKey(k1) error = %v", err)
		}
		if !engine.VerifyToken(tok1, "user@example.com") || !engine.VerifyToken(tok3, "user@example.com") {
			t.Fatalf("expected both k1 and k3 tokens to verify")
		}
	})

	t.Run("errors on invalid env config", func(t *testing.T) {
		t.Setenv("DOCTOR_PRIVACY_KEY_ID", "")
		t.Setenv("DOCTOR_PRIVACY_HMAC_KEY", "")
		t.Setenv("DOCTOR_PRIVACY_KEYS_JSON", "")
		if _, err := LoadFromEnv(); err == nil {
			t.Fatal("expected error when no privacy keys are configured")
		}

		t.Setenv("DOCTOR_PRIVACY_KEYS_JSON", "{invalid-json")
		if _, err := LoadFromEnv(); err == nil {
			t.Fatal("expected error for invalid DOCTOR_PRIVACY_KEYS_JSON")
		}
	})
}
