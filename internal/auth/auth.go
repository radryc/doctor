// Package auth wires OIDC SSO and bearer authentication into doctor-query.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

// Config holds the authentication settings loaded from the environment.
type Config struct {
	Issuer          string
	Audience        string
	JWKSURL         string
	ClientID        string
	ClientSecret    string
	RedirectURL     string
	EndSessionURL   string
	AuthURL         string
	Scopes          []string
	SessionDir      string
	BreakGlassToken string
	Require         bool
	RequireLogin    bool
	ExemptPaths     []string
}

// Enabled reports whether any verifier is configured.
func (c Config) Enabled() bool {
	return (c.Issuer != "" && c.Audience != "") || c.BreakGlassToken != ""
}

// BrowserLoginEnabled reports whether the OIDC authorization-code flow can run.
func (c Config) BrowserLoginEnabled() bool {
	return c.Issuer != "" && c.Audience != "" && c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

// NewVerifier builds the verifier chain: OIDC first, then the break-glass token.
// It returns (nil, nil) when authentication is not configured.
func NewVerifier(cfg Config) (authz.TokenVerifier, error) {
	verifiers := make([]authz.TokenVerifier, 0, 2)
	if cfg.Issuer != "" && cfg.Audience != "" {
		verifier, err := authz.NewOIDCVerifier(authz.OIDCConfig{
			Issuer:   cfg.Issuer,
			Audience: cfg.Audience,
			JWKSURL:  cfg.JWKSURL,
		})
		if err != nil {
			return nil, fmt.Errorf("auth: build OIDC verifier: %w", err)
		}
		verifiers = append(verifiers, verifier)
	}
	if cfg.BreakGlassToken != "" {
		verifiers = append(verifiers, authz.NewBreakGlassVerifier(cfg.BreakGlassToken, "break-glass-admin"))
	}
	if len(verifiers) == 0 {
		return nil, nil
	}
	return authz.NewChainVerifier(verifiers...), nil
}

// NewWebAuthenticator builds the browser login handler. It returns (nil, nil)
// when browser login is not configured.
func NewWebAuthenticator(ctx context.Context, cfg Config, verifier authz.TokenVerifier) (*authz.WebAuthenticator, error) {
	if !cfg.BrowserLoginEnabled() {
		return nil, nil
	}
	exempt := cfg.ExemptPaths
	if len(exempt) == 0 {
		exempt = DefaultExemptPaths()
	}
	webCfg := authz.WebAuthConfig{
		Issuer:            cfg.Issuer,
		ClientID:          cfg.ClientID,
		ClientSecret:      cfg.ClientSecret,
		RedirectURL:       cfg.RedirectURL,
		EndSessionURL:     cfg.EndSessionURL,
		Scopes:            cfg.Scopes,
		Verifier:          verifier,
		RequireLogin:      cfg.RequireLogin || cfg.Require,
		CookieName:        "doctor_session",
		PostLoginRedirect: "/",
		ExemptPaths:       exempt,
	}
	if strings.TrimSpace(cfg.AuthURL) != "" {
		webCfg.Endpoints.AuthURL = strings.TrimSpace(cfg.AuthURL)
	}
	if dir := strings.TrimSpace(cfg.SessionDir); dir != "" {
		sessions, err := authz.NewPersistentSessionStore(filepath.Clean(dir), 12*time.Hour)
		if err != nil {
			return nil, fmt.Errorf("auth: open session store: %w", err)
		}
		webCfg.Sessions = sessions
	}
	return authz.NewWebAuthenticator(ctx, webCfg)
}

// DefaultExemptPaths are always reachable without a bearer token. Metrics
// endpoints stay public by design; logs and traces are gated by the access
// layer instead.
func DefaultExemptPaths() []string {
	return []string{"/healthz", "/ready", "/api/echo", "/metrics", "/api/v1"}
}

// Middleware attaches identity to requests and optionally rejects anonymous
// callers outside the exempt paths.
type Middleware struct {
	authenticator *authz.Authenticator
	require       bool
	exempt        []string
}

// NewMiddleware builds the authentication middleware. A nil verifier returns a
// pass-through middleware.
func NewMiddleware(verifier authz.TokenVerifier, require bool, exemptPaths []string, logger *log.Logger) *Middleware {
	if verifier == nil {
		return &Middleware{}
	}
	exempt := exemptPaths
	if len(exempt) == 0 {
		exempt = DefaultExemptPaths()
	}
	return &Middleware{
		authenticator: authz.NewAuthenticator(verifier, nil, false),
		require:       require,
		exempt:        exempt,
	}
}

// Wrap implements the HTTP middleware.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	if m == nil || m.authenticator == nil {
		return next
	}
	return m.authenticator.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.require && !m.isExempt(r.URL.Path) {
			if identity, ok := authz.IdentityFromContext(r.Context()); !ok || identity.IsAnonymous() {
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":"error","errorType":"unauthorized"}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	}))
}

func (m *Middleware) isExempt(path string) bool {
	for _, exempt := range m.exempt {
		if exempt == "" {
			continue
		}
		if path == exempt {
			return true
		}
		if strings.HasSuffix(exempt, "/") && strings.HasPrefix(path, exempt) {
			return true
		}
		if strings.HasPrefix(path, exempt+"/") {
			return true
		}
	}
	return false
}

// ErrNotConfigured reports that authentication was requested but no verifier
// exists.
var ErrNotConfigured = errors.New("auth: no verifier configured")
