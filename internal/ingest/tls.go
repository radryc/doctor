package ingest

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
)

// CollectorMTLS configures client-certificate verification for one listener.
// Mode is off (default), optional, or require.
type CollectorMTLS struct {
	Mode   string
	CAFile string
}

// NormalizedMode returns the lower-case mode, defaulting to off.
func (m CollectorMTLS) NormalizedMode() string {
	mode := strings.ToLower(strings.TrimSpace(m.Mode))
	if mode == "" {
		return "off"
	}
	return mode
}

// Validate checks that a non-off mode has a CA bundle.
func (m CollectorMTLS) Validate() error {
	switch m.NormalizedMode() {
	case "off":
		return nil
	case "optional", "require":
		if strings.TrimSpace(m.CAFile) == "" {
			return fmt.Errorf("mTLS mode %q requires a client CA file", m.NormalizedMode())
		}
		return nil
	default:
		return fmt.Errorf("unknown mTLS mode %q (want off, optional, or require)", m.Mode)
	}
}

// CollectorTLSConfig builds a server TLS config for OTLP HTTP/gRPC. A nil
// config means the listener is plaintext. When mTLS is enabled, collectors must
// present a certificate signed by CAFile (optional also verifies when one is
// presented).
func CollectorTLSConfig(certFile, keyFile string, mtls CollectorMTLS) (*tls.Config, error) {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if certFile == "" && keyFile == "" {
		if mtls.NormalizedMode() != "off" {
			return nil, fmt.Errorf("mTLS mode %q requires TLS certificate and key files", mtls.NormalizedMode())
		}
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("both TLS certificate and key files are required")
	}
	if err := mtls.Validate(); err != nil {
		return nil, err
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}
	switch mtls.NormalizedMode() {
	case "off":
		config.ClientAuth = tls.NoClientCert
	case "optional":
		pool, err := loadClientCAs(mtls.CAFile)
		if err != nil {
			return nil, err
		}
		config.ClientAuth = tls.VerifyClientCertIfGiven
		config.ClientCAs = pool
	case "require":
		pool, err := loadClientCAs(mtls.CAFile)
		if err != nil {
			return nil, err
		}
		config.ClientAuth = tls.RequireAndVerifyClientCert
		config.ClientCAs = pool
	}
	return config, nil
}

func loadClientCAs(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("read client CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("client CA file %s contains no valid PEM certificates", path)
	}
	return pool, nil
}
