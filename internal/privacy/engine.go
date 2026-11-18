package privacy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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
	ibanPattern           = regexp.MustCompile(`\b[A-Za-z]{2}\d{2}[A-Za-z0-9]{10,30}\b`)
	routingPattern        = regexp.MustCompile(`\b\d{9}\b`)
	phonePattern          = regexp.MustCompile(`(?:\+[1-9]\d{7,14}\b|\b(?:1[-. ]?)?\(?\d{3}\)?[-. ]\d{3}[-. ]\d{4}\b)`)
	cvvPattern            = regexp.MustCompile(`(?i)\b(?:cvv2?|cvc2?|card[ _-]?verification(?:[ _-](?:code|value))?)\b\s*[:=#]?\s*\d{3,4}\b`)
	dobPattern            = regexp.MustCompile(`(?i)\b(?:dob|d\.o\.b\.?|date[ _-]?of[ _-]?birth|birth[ _-]?(?:date|day))\b\s*[:=]?\s*(?:\d{4}[-/.]\d{1,2}[-/.]\d{1,2}|\d{1,2}[-/.]\d{1,2}[-/.]\d{2,4})\b`)
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

const (
	defaultKeyID  = "k1"
	tokenPrefix   = "tok_v2_"
	tokenTagBytes = 16 // 128-bit MAC tag = 32 hex chars
)

type Engine struct {
	activeKeyID string
	keys        map[string][]byte
}

func New(hmacKey string) (*Engine, error) {
	if strings.TrimSpace(hmacKey) == "" {
		return nil, fmt.Errorf("privacy hmac key is required")
	}
	return NewWithKeys(defaultKeyID, map[string]string{defaultKeyID: hmacKey})
}

// NewWithKeys constructs a privacy engine with an active key ID and a map of
// key ID -> HMAC secret for key rotation and historical token verification.
func NewWithKeys(activeKeyID string, keys map[string]string) (*Engine, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("privacy hmac key is required")
	}
	normalized := make(map[string][]byte, len(keys))
	for rawID, secret := range keys {
		kid := sanitizeKeyID(rawID)
		if kid == "" {
			return nil, fmt.Errorf("invalid privacy key id %q", rawID)
		}
		if strings.TrimSpace(secret) == "" {
			return nil, fmt.Errorf("privacy hmac key for %q is empty", kid)
		}
		if _, exists := normalized[kid]; exists {
			return nil, fmt.Errorf("duplicate privacy key id %q after sanitization", kid)
		}
		normalized[kid] = []byte(secret)
	}

	active := defaultKeyID
	if strings.TrimSpace(activeKeyID) != "" {
		active = sanitizeKeyID(activeKeyID)
		if active == "" {
			return nil, fmt.Errorf("invalid active privacy key id %q", activeKeyID)
		}
	}
	if _, ok := normalized[active]; !ok {
		return nil, fmt.Errorf("active privacy key id %q not found in keys", active)
	}

	return &Engine{
		activeKeyID: active,
		keys:        normalized,
	}, nil
}

// LoadFromEnv builds an Engine from DOCTOR_PRIVACY_KEY_ID,
// DOCTOR_PRIVACY_HMAC_KEY, and DOCTOR_PRIVACY_KEYS_JSON, falling back to a
// single "k1" key when only DOCTOR_PRIVACY_HMAC_KEY is set.
func LoadFromEnv() (*Engine, error) {
	rawKeyID := strings.TrimSpace(os.Getenv("DOCTOR_PRIVACY_KEY_ID"))
	hmacKey := os.Getenv("DOCTOR_PRIVACY_HMAC_KEY")
	keysJSON := strings.TrimSpace(os.Getenv("DOCTOR_PRIVACY_KEYS_JSON"))

	activeKeyID := defaultKeyID
	if rawKeyID != "" {
		activeKeyID = sanitizeKeyID(rawKeyID)
		if activeKeyID == "" {
			return nil, fmt.Errorf("invalid DOCTOR_PRIVACY_KEY_ID %q", rawKeyID)
		}
	}

	keys := make(map[string]string)
	if keysJSON != "" {
		var parsed map[string]string
		if err := json.Unmarshal([]byte(keysJSON), &parsed); err != nil {
			return nil, fmt.Errorf("parse DOCTOR_PRIVACY_KEYS_JSON: %w", err)
		}
		for k, v := range parsed {
			keys[k] = v
		}
	}

	if strings.TrimSpace(hmacKey) != "" {
		if _, exists := keys[activeKeyID]; !exists {
			keys[activeKeyID] = hmacKey
		}
	}

	if len(keys) == 0 {
		return New(hmacKey)
	}
	return NewWithKeys(activeKeyID, keys)
}

func sanitizeKeyID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		}
	}
	return b.String()
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
	text = cvvPattern.ReplaceAllString(text, "[REDACTED]")
	text = dobPattern.ReplaceAllString(text, "[REDACTED]")
	text = phonePattern.ReplaceAllStringFunc(text, func(value string) string {
		return e.Tokenize(value)
	})
	text = ibanPattern.ReplaceAllStringFunc(text, func(value string) string {
		if isValidIBAN(value) {
			return e.Tokenize(value)
		}
		return value
	})
	text = routingPattern.ReplaceAllStringFunc(text, func(value string) string {
		if isValidABARouting(value) {
			return e.Tokenize(value)
		}
		return value
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
	return tokenizeWithSecret(e.activeKeyID, e.keys[e.activeKeyID], value)
}

// TokenizeWithKey mints a versioned 128-bit HMAC token using the specified key ID.
func (e *Engine) TokenizeWithKey(keyID, value string) (string, error) {
	kid := sanitizeKeyID(keyID)
	key, ok := e.keys[kid]
	if !ok {
		return "", fmt.Errorf("unknown privacy key id %q", keyID)
	}
	return tokenizeWithSecret(kid, key, value), nil
}

// VerifyToken checks whether token is a valid versioned HMAC token for rawValue
// using any configured key (active or rotated).
func (e *Engine) VerifyToken(token, rawValue string) bool {
	if !strings.HasPrefix(token, tokenPrefix) {
		return false
	}
	rest := strings.TrimPrefix(token, tokenPrefix)
	sep := strings.IndexByte(rest, '_')
	if sep <= 0 {
		return false
	}
	kid := rest[:sep]
	tagHex := rest[sep+1:]
	if len(tagHex) != tokenTagBytes*2 {
		return false
	}
	tag, err := hex.DecodeString(tagHex)
	if err != nil || len(tag) != tokenTagBytes {
		return false
	}
	key, ok := e.keys[kid]
	if !ok || len(key) == 0 {
		return false
	}
	return hmac.Equal(tag, computeTag(key, rawValue))
}

func tokenizeWithSecret(keyID string, key []byte, value string) string {
	return tokenPrefix + keyID + "_" + hex.EncodeToString(computeTag(key, value))
}

func computeTag(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)[:tokenTagBytes]
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

// isValidIBAN normalises a candidate IBAN and checks the ISO 13616 mod-97
// checksum so ordinary identifiers are not tokenized by mistake.
func isValidIBAN(value string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(value, " ", ""), "-", ""))
	if len(normalized) < 15 || len(normalized) > 34 {
		return false
	}
	for i := 0; i < len(normalized); i++ {
		c := normalized[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	rearranged := normalized[4:] + normalized[:4]
	mod := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		if c >= '0' && c <= '9' {
			mod = (mod*10 + int(c-'0')) % 97
			continue
		}
		mod = (mod*100 + int(c-'A') + 10) % 97
	}
	return mod == 1
}

// isValidABARouting checks the ABA routing-number checksum.
func isValidABARouting(value string) bool {
	if len(value) != 9 {
		return false
	}
	digits := make([]int, 9)
	for i := 0; i < 9; i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
		digits[i] = int(value[i] - '0')
	}
	sum := 3*(digits[0]+digits[3]+digits[6]) + 7*(digits[1]+digits[4]+digits[7]) + digits[2] + digits[5] + digits[8]
	return sum%10 == 0
}
