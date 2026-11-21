package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

// InternalIdentityHeader carries a short-lived, HMAC-signed identity between
// Doctor services (for example doctor-query -> doctor-rules).
const InternalIdentityHeader = "X-Doctor-Identity"

type internalIdentityClaims struct {
	Subject  string   `json:"sub,omitempty"`
	ClientID string   `json:"client_id,omitempty"`
	Groups   []string `json:"groups,omitempty"`
	Expires  int64    `json:"exp"`
}

// SignInternalIdentity builds a compact HMAC-signed identity token. The secret
// is shared only between the two services; callers cannot forge an identity
// even if they can reach the downstream service.
func SignInternalIdentity(secret string, identity authz.Identity, ttl time.Duration, now time.Time) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("auth: internal identity secret is empty")
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	claims := internalIdentityClaims{
		Subject:  identity.Subject,
		ClientID: identity.ClientID,
		Groups:   append([]string(nil), identity.Groups...),
		Expires:  now.Add(ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + signInternalIdentity(secret, encoded), nil
}

// VerifyInternalIdentity validates a token produced by SignInternalIdentity.
func VerifyInternalIdentity(secret, token string, now time.Time) (authz.Identity, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return authz.Identity{}, fmt.Errorf("auth: internal identity secret is empty")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return authz.Identity{}, fmt.Errorf("auth: malformed internal identity")
	}
	expected := signInternalIdentity(secret, parts[0])
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return authz.Identity{}, fmt.Errorf("auth: invalid internal identity signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return authz.Identity{}, fmt.Errorf("auth: decode internal identity: %w", err)
	}
	var claims internalIdentityClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return authz.Identity{}, fmt.Errorf("auth: parse internal identity: %w", err)
	}
	if claims.Expires <= now.Unix() {
		return authz.Identity{}, fmt.Errorf("auth: internal identity expired")
	}
	identity := authz.Identity{
		Subject:  claims.Subject,
		ClientID: claims.ClientID,
		Groups:   claims.Groups,
	}
	if identity.IsAnonymous() {
		return authz.Identity{}, fmt.Errorf("auth: internal identity is anonymous")
	}
	return identity, nil
}

// InternalIdentityMiddleware replaces the request identity with a verified,
// signed identity when the header is present. An invalid header is rejected
// rather than silently falling back to the service credential.
func InternalIdentityMiddleware(secret string, logger *log.Logger) func(http.Handler) http.Handler {
	secret = strings.TrimSpace(secret)
	return func(next http.Handler) http.Handler {
		if secret == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := strings.TrimSpace(r.Header.Get(InternalIdentityHeader))
			if raw == "" {
				next.ServeHTTP(w, r)
				return
			}
			identity, err := VerifyInternalIdentity(secret, raw, time.Now().UTC())
			if err != nil {
				if logger != nil {
					logger.Printf("auth: reject internal identity: %v", err)
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":"error","errorType":"unauthorized"}`))
				return
			}
			// Remove the header before forwarding so it cannot leak upstream.
			r.Header.Del(InternalIdentityHeader)
			next.ServeHTTP(w, r.WithContext(authz.ContextWithIdentity(r.Context(), identity)))
		})
	}
}

func signInternalIdentity(secret, encoded string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// IdentityFromContext is a small helper for callers that only need the
// identity and do not want to import monofs authz directly.
func IdentityFromContext(ctx context.Context) (authz.Identity, bool) {
	return authz.IdentityFromContext(ctx)
}
