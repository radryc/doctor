package privacy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var (
	sensitiveKeyPattern   = regexp.MustCompile(`(?i)(password|passwd|secret|token|authorization|cookie|session|apikey|api_key|jwt|bearer|credential)`)
	tokenizableKeyPattern = regexp.MustCompile(`(?i)(email|user(_?id)?|account(_?id)?|client(_?id)?|ip|remote_addr|remoteaddr)`)
	emailPattern          = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	ipv4Pattern           = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	bearerPattern         = regexp.MustCompile(`(?i)bearer\s+[a-z0-9\-_\.=]+`)
	jwtPattern            = regexp.MustCompile(`^[A-Za-z0-9\-_]{8,}\.[A-Za-z0-9\-_]{8,}\.[A-Za-z0-9\-_]{8,}$`)
	longSecretPattern     = regexp.MustCompile(`(?i)\b(?:sk|pk|api|tok|secret)[a-z0-9_\-]{8,}\b`)
)

type Engine struct {
	key []byte
}

func New(hmacKey string) (*Engine, error) {
	if strings.TrimSpace(hmacKey) == "" {
		return nil, fmt.Errorf("privacy hmac key is required")
	}
	return &Engine{key: []byte(hmacKey)}, nil
}

func (e *Engine) SanitizeAttributes(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for key, value := range attrs {
		out[key] = e.sanitizeValue(key, value)
	}
	return out
}

func (e *Engine) SanitizeText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	if looksSensitive(text) {
		return "[REDACTED]"
	}
	text = emailPattern.ReplaceAllStringFunc(text, func(value string) string {
		return e.Tokenize(value)
	})
	text = ipv4Pattern.ReplaceAllStringFunc(text, func(value string) string {
		return e.Tokenize(value)
	})
	return text
}

func (e *Engine) Tokenize(value string) string {
	mac := hmac.New(sha256.New, e.key)
	_, _ = mac.Write([]byte(value))
	sum := mac.Sum(nil)
	return "tok_" + hex.EncodeToString(sum[:8])
}

func (e *Engine) sanitizeValue(key, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	switch {
	case sensitiveKeyPattern.MatchString(key), looksSensitive(value):
		return "[REDACTED]"
	case tokenizableKeyPattern.MatchString(key):
		return e.Tokenize(value)
	default:
		return e.SanitizeText(value)
	}
}

func looksSensitive(value string) bool {
	return bearerPattern.MatchString(value) || jwtPattern.MatchString(value) || longSecretPattern.MatchString(value)
}
