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
	sensitiveKeyPattern   = regexp.MustCompile(`(?i)(password|passwd|secret|token|authorization|cookie|session|apikey|api_key|jwt|bearer|credential|card_number|cardnumber|routing_number|passport|phone|(?:^|[^a-z0-9])(?:pan|cvv|cvc|ssn|tin|iban|pin|dob)(?:$|[^a-z0-9]))`)
	tokenizableKeyPattern = regexp.MustCompile(`(?i)(email|user(_?id)?|account(_?id)?|client(_?id)?|ip|remote_addr|remoteaddr)`)
	emailPattern          = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	ipv4Pattern           = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ssnPattern            = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	panPattern            = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)
	bearerPattern         = regexp.MustCompile(`(?i)bearer\s+[a-z0-9\-_\.=]+`)
	// A JWT always has a base64url header that starts with "eyJ" ({"...).
	// Requiring it prevents canonical service names such as
	// guardian-configs.guardian.guardiand from being mistaken for tokens.
	jwtPattern        = regexp.MustCompile(`\beyJ[A-Za-z0-9\-_]{8,}\.[A-Za-z0-9\-_]{8,}\.[A-Za-z0-9\-_]{16,}`)
	longSecretPattern = regexp.MustCompile(`(?i)\b(?:(?:sk|pk)[-_][a-z0-9_-]{8,}|(?:api|tok|secret)[-_][a-z0-9_-]*[0-9][a-z0-9_-]{6,}|akia[0-9a-z]{16}|ghp_[a-z0-9]{36}|xox[baprs]-[a-z0-9-]{10,})\b`)
)

// identityKeys are structural telemetry identity fields. They must never be
// redacted: doing so makes a service disappear from the APM catalog and breaks
// log/trace/metric correlation.
var identityKeys = map[string]struct{}{
	"service.name":               {},
	"service.namespace":          {},
	"service.alias":              {},
	"service.full_name":          {},
	"service_full":               {},
	"service_alias":              {},
	"guardian.partition":         {},
	"guardian.intent":            {},
	"guardian.asset":             {},
	"guardian.asset.type":        {},
	"guardian.service.full_name": {},
	"guardian.identity.kind":     {},
	"k8s.namespace.name":         {},
	"k8s.pod.name":               {},
	"k8s.container.name":         {},
	"k8s.node.name":              {},
}

func isIdentityKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if _, ok := identityKeys[key]; ok {
		return true
	}
	return strings.HasPrefix(key, "guardian.target.")
}

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
	text = panPattern.ReplaceAllStringFunc(text, func(value string) string {
		if isValidLuhn(value) {
			return e.Tokenize(value)
		}
		return value
	})
	text = ssnPattern.ReplaceAllStringFunc(text, func(value string) string {
		return e.Tokenize(value)
	})
	return text
}

func isValidLuhn(s string) bool {
	var digits []int
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			digits = append(digits, int(c-'0'))
		} else if c != ' ' && c != '-' {
			return false
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
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
	case isIdentityKey(key):
		return value
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
