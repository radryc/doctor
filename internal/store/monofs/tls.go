package monofs

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"strings"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// TLSConfig controls transport security for MonoFS router and node connections.
// It is loaded from environment variables so every doctor binary gets the same
// behaviour without duplicating config plumbing.
type TLSConfig struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
	// Require rejects plaintext connections even when no certificate files are
	// configured (system roots are used).
	Require bool
	// Insecure is the explicit development-only escape hatch.
	Insecure bool
}

// TLSConfigFromEnv reads the shared MonoFS TLS settings.
func TLSConfigFromEnv() TLSConfig {
	return TLSConfig{
		CAFile:     strings.TrimSpace(os.Getenv("DOCTOR_MONOFS_TLS_CA")),
		CertFile:   strings.TrimSpace(os.Getenv("DOCTOR_MONOFS_TLS_CERT")),
		KeyFile:    strings.TrimSpace(os.Getenv("DOCTOR_MONOFS_TLS_KEY")),
		ServerName: strings.TrimSpace(os.Getenv("DOCTOR_MONOFS_TLS_SERVER_NAME")),
		Require:    envBool("DOCTOR_MONOFS_TLS_REQUIRE", false),
		Insecure:   envBool("DOCTOR_MONOFS_TLS_INSECURE", false),
	}
}

// Credentials builds the gRPC transport credentials. Plaintext is returned
// only when TLS is neither required nor configured and the explicit insecure
// flag (or development default) applies.
func (c TLSConfig) Credentials() (credentials.TransportCredentials, error) {
	if c.Insecure && c.Require {
		return nil, fmt.Errorf("monofs TLS: DOCTOR_MONOFS_TLS_INSECURE and DOCTOR_MONOFS_TLS_REQUIRE are mutually exclusive")
	}
	if c.Insecure {
		log.Printf("monofs TLS: using plaintext transport because DOCTOR_MONOFS_TLS_INSECURE=true")
		return insecure.NewCredentials(), nil
	}
	if !c.Require && c.CAFile == "" && c.CertFile == "" && c.KeyFile == "" {
		return insecure.NewCredentials(), nil
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return nil, fmt.Errorf("monofs TLS: client certificate and key must be configured together")
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: c.ServerName,
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("monofs TLS: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("monofs TLS: parse CA bundle %s", c.CAFile)
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("monofs TLS: load client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(cfg), nil
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
