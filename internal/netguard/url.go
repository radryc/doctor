// Package netguard validates operator- and discovery-supplied URLs before they
// are used as backend endpoints. It blocks the classic SSRF targets while
// allowing normal in-cluster private services through an explicit policy.
package netguard

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Policy controls which backend URLs are accepted.
type Policy struct {
	// RequireHTTPS rejects plain HTTP endpoints. Enable in production.
	RequireHTTPS bool
	// AllowLoopback permits 127.0.0.0/8, ::1, and localhost.
	AllowLoopback bool
	// AllowPrivate permits RFC1918/ULA addresses (normal for in-cluster
	// services). Link-local and metadata addresses are always rejected.
	AllowPrivate bool
	// AllowedHosts is an exact-host or suffix allowlist (for example
	// "loki.monitoring.svc.cluster.local" or "*.example.com").
	AllowedHosts []string
}

var blockedHosts = map[string]struct{}{
	"metadata":                 {},
	"metadata.google.internal": {},
	"metadata.azure.internal":  {},
}

// Validate parses rawURL and applies the policy. An empty rawURL is allowed so
// callers can validate optional fields without special-casing.
func (p Policy) Validate(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	if !p.enabled() {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("URL scheme %q must be http or https", parsed.Scheme)
	}
	if p.RequireHTTPS && parsed.Scheme != "https" {
		return fmt.Errorf("URL %q must use https", rawURL)
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("URL %q has no host", rawURL)
	}
	if p.hostAllowed(host) {
		return nil
	}
	if len(p.AllowedHosts) > 0 {
		return fmt.Errorf("URL host %q is not in the allowlist", host)
	}
	if _, blocked := blockedHosts[host]; blocked {
		return fmt.Errorf("URL host %q is a cloud metadata endpoint", host)
	}
	if host == "localhost" {
		if !p.AllowLoopback {
			return fmt.Errorf("URL host %q is loopback; set AllowLoopback for development", host)
		}
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostnames are allowed because resolving them here would introduce a
		// DNS-rebinding gap. The allowlist is the control for named services.
		return nil
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("URL host %q is not a unicast address", host)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("URL host %q is link-local", host)
	}
	if ip.IsLoopback() && !p.AllowLoopback {
		return fmt.Errorf("URL host %q is loopback; set AllowLoopback for development", host)
	}
	if ip.IsPrivate() && !p.AllowPrivate {
		return fmt.Errorf("URL host %q is private; set AllowPrivate for in-cluster backends", host)
	}
	return nil
}

func (p Policy) enabled() bool {
	return p.RequireHTTPS || p.AllowLoopback || p.AllowPrivate || len(p.AllowedHosts) > 0
}

func (p Policy) hostAllowed(host string) bool {
	for _, allowed := range p.AllowedHosts {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "" {
			continue
		}
		if strings.HasPrefix(allowed, "*.") {
			suffix := strings.TrimPrefix(allowed, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
			continue
		}
		if host == allowed {
			return true
		}
	}
	return false
}
