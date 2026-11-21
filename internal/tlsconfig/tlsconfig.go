// Package tlsconfig adapts the shared STRATA_TLS_* contract
// (github.com/radryc/tlsx) to doctor's listeners and outbound HTTP clients.
//
// STRATA_TLS_MODE defaults to "off", which keeps every existing DOCTOR_* TLS
// behaviour byte-for-byte unchanged: listeners stay plaintext unless the
// legacy DOCTOR_TLS_* certificate pair is configured, and outbound clients use
// the default transport.
package tlsconfig

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/radryc/tlsx"
)

// Config is the process-wide STRATA_TLS_* contract.
type Config struct {
	Env tlsx.EnvConfig
}

// Mode returns the configured STRATA_TLS_MODE.
func (c Config) Mode() tlsx.Mode { return c.Env.Mode }

// Enabled reports whether TLS is configured at all.
func (c Config) Enabled() bool { return c.Env.Mode != tlsx.ModeOff }

// Required reports whether plaintext is forbidden.
func (c Config) Required() bool { return c.Env.Mode == tlsx.ModeRequired }

// Load reads and validates STRATA_TLS_*.
func Load() (Config, error) {
	env, err := tlsx.LoadEnv()
	if err != nil {
		return Config{}, err
	}
	return Config{Env: env}, nil
}

var (
	defaultOnce sync.Once
	defaultCfg  Config
	defaultErr  error
)

// Default returns the cached process-wide configuration. Parsing errors are
// cached too so a malformed contract cannot silently degrade to plaintext
// between calls.
func Default() (Config, error) {
	defaultOnce.Do(func() {
		defaultCfg, defaultErr = Load()
	})
	return defaultCfg, defaultErr
}

// ServerTLS builds the TLS configuration for an HTTP or gRPC listener.
//
// With STRATA_TLS_MODE=off the legacy certificate pair is honoured exactly as
// before: no pair means plaintext, a pair means one-way TLS. With
// optional/required the STRATA_TLS_* certificate is used and client
// certificates are verified according to STRATA_TLS_REQUIRE_CLIENT_CERT.
func (c Config) ServerTLS(legacyCertFile, legacyKeyFile string) (*tls.Config, error) {
	legacyCertFile = strings.TrimSpace(legacyCertFile)
	legacyKeyFile = strings.TrimSpace(legacyKeyFile)

	if !c.Enabled() {
		if legacyCertFile == "" && legacyKeyFile == "" {
			return nil, nil
		}
		if legacyCertFile == "" || legacyKeyFile == "" {
			return nil, errors.New("both the TLS certificate and key files are required")
		}
		return tlsx.NewServerTLSConfig(tlsx.ServerConfig{
			CertFile: legacyCertFile,
			KeyFile:  legacyKeyFile,
		})
	}

	serverCfg := c.Env.ServerConfig()
	if strings.TrimSpace(serverCfg.CertFile) == "" || strings.TrimSpace(serverCfg.KeyFile) == "" {
		return nil, errors.New("tlsx: STRATA_TLS_CERT_FILE and STRATA_TLS_KEY_FILE are required when STRATA_TLS_MODE is not off")
	}
	if c.Env.Mode == tlsx.ModeOptional && serverCfg.CAFile != "" && !fileExists(serverCfg.CAFile) {
		// Optional mode is the migration mode: a missing client CA degrades to
		// one-way TLS instead of refusing to start. Required mode still fails
		// closed inside tlsx.
		log.Printf("tlsx: optional mode without client CA at %s; serving one-way TLS", serverCfg.CAFile)
		serverCfg.CAFile = ""
		serverCfg.RequireClientCert = false
	}
	return tlsx.NewServerTLSConfig(serverCfg)
}

// ServerTLSHTTP builds the TLS configuration for an HTTP listener. Optional
// mode keeps HTTP plaintext so local UIs, port-forwards and probes keep
// working while gRPC uses mTLS; required mode enables TLS for HTTP too. An
// explicit legacy certificate is honored as an opt-in.
func (c Config) ServerTLSHTTP(legacyCertFile, legacyKeyFile string) (*tls.Config, error) {
	if c.Mode() == tlsx.ModeOptional && strings.TrimSpace(legacyCertFile) == "" {
		return nil, nil
	}
	return c.ServerTLS(legacyCertFile, legacyKeyFile)
}

// ClientTLS builds the TLS configuration used for outbound calls. The client
// certificate is optional (one-way TLS when absent); the CA bundle is used
// whenever the file exists.
func (c Config) ClientTLS() (*tls.Config, error) {
	if !c.Enabled() {
		return nil, nil
	}
	clientCfg := c.Env.ClientConfig()
	clientCfg.CertFile, clientCfg.KeyFile = clientPair(clientCfg.CertFile, clientCfg.KeyFile)
	if clientCfg.CAFile != "" && !fileExists(clientCfg.CAFile) {
		if c.Required() {
			return nil, fmt.Errorf("tlsx: STRATA_TLS_CA_FILE %q is not readable in required mode", clientCfg.CAFile)
		}
		clientCfg.CAFile = ""
	}
	return tlsx.NewClientTLSConfig(clientCfg)
}

// Transport applies the client TLS configuration to an HTTP transport. When
// TLS is off the base transport is returned unchanged (a nil base means the
// standard library default).
func (c Config) Transport(base *http.Transport) *http.Transport {
	if !c.Enabled() {
		return base
	}
	if base == nil {
		std, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			base = std.Clone()
		} else {
			base = &http.Transport{}
		}
	} else {
		base = base.Clone()
	}
	tlsCfg, err := c.ClientTLS()
	if err != nil {
		if c.Required() {
			// Fail closed: keep TLS mandatory even though the trust bundle
			// could not be loaded.
			log.Printf("tlsx: client TLS configuration failed (outbound calls will fail closed): %v", err)
			base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			return base
		}
		log.Printf("tlsx: ignoring client TLS configuration: %v", err)
		return base
	}
	if tlsCfg != nil {
		base.TLSClientConfig = tlsCfg
	}
	return base
}

// HTTPClient returns an *http.Client wired for the STRATA_TLS_* contract.
func (c Config) HTTPClient(timeout time.Duration) *http.Client {
	if !c.Enabled() {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{Timeout: timeout, Transport: c.Transport(nil)}
}

// URL upgrades http:// to https:// in required mode. Other schemes are left
// untouched.
func (c Config) URL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !c.Required() {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" {
		return raw
	}
	parsed.Scheme = "https"
	return parsed.String()
}

// HTTPClient returns a client for the cached process configuration. When the
// contract cannot be loaded it logs once and returns a plaintext client; server
// startup fails before that code path is reached.
func HTTPClient(timeout time.Duration) *http.Client {
	cfg, err := Default()
	if err != nil {
		log.Printf("tlsx: using the default HTTP client because STRATA_TLS_* is invalid: %v", err)
		return &http.Client{Timeout: timeout}
	}
	return cfg.HTTPClient(timeout)
}

// Transport returns an HTTP transport for the cached process configuration.
// It never returns nil so callers can assign it to http.RoundTripper fields.
func Transport() *http.Transport {
	cfg, err := Default()
	if err != nil {
		return defaultTransport()
	}
	if transport := cfg.Transport(nil); transport != nil {
		return transport
	}
	return defaultTransport()
}

func defaultTransport() *http.Transport {
	if std, ok := http.DefaultTransport.(*http.Transport); ok {
		return std.Clone()
	}
	return &http.Transport{}
}

// URL upgrades an outbound URL using the cached process configuration.
func URL(raw string) string {
	cfg, err := Default()
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return cfg.URL(raw)
}

func clientPair(certFile, keyFile string) (string, string) {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if certFile == "" || keyFile == "" || !fileExists(certFile) || !fileExists(keyFile) {
		return "", ""
	}
	return certFile, keyFile
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
