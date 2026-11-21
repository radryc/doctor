package ingest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testCollectorCertificates(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "collector"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	caFile = filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, caFile
}

func TestCollectorTLSConfigModes(t *testing.T) {
	certFile, keyFile, caFile := testCollectorCertificates(t)
	cases := []struct {
		name     string
		mtls     CollectorMTLS
		wantAuth tls.ClientAuthType
		wantCAs  bool
		wantErr  bool
	}{
		{name: "off", mtls: CollectorMTLS{Mode: "off"}, wantAuth: tls.NoClientCert},
		{name: "optional", mtls: CollectorMTLS{Mode: "optional", CAFile: caFile}, wantAuth: tls.VerifyClientCertIfGiven, wantCAs: true},
		{name: "require", mtls: CollectorMTLS{Mode: "require", CAFile: caFile}, wantAuth: tls.RequireAndVerifyClientCert, wantCAs: true},
		{name: "require-missing-ca", mtls: CollectorMTLS{Mode: "require"}, wantErr: true},
		{name: "invalid-mode", mtls: CollectorMTLS{Mode: "sometimes", CAFile: caFile}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, err := CollectorTLSConfig(certFile, keyFile, tc.mtls)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("CollectorTLSConfig: %v", err)
			}
			if config == nil || config.ClientAuth != tc.wantAuth {
				t.Fatalf("config = %#v, want ClientAuth=%v", config, tc.wantAuth)
			}
			if tc.wantCAs && config.ClientCAs == nil {
				t.Fatal("expected ClientCAs")
			}
			if config.MinVersion != tls.VersionTLS12 {
				t.Fatalf("MinVersion = %v", config.MinVersion)
			}
		})
	}
}

func TestCollectorTLSConfigRequiresCertPair(t *testing.T) {
	if _, err := CollectorTLSConfig("", "", CollectorMTLS{Mode: "require", CAFile: "ca.pem"}); err == nil {
		t.Fatal("expected mTLS without a server certificate to fail")
	}
	if _, err := CollectorTLSConfig("server.crt", "", CollectorMTLS{}); err == nil {
		t.Fatal("expected a half-configured certificate pair to fail")
	}
	if config, err := CollectorTLSConfig("", "", CollectorMTLS{}); err != nil || config != nil {
		t.Fatalf("plaintext config = %#v, err = %v", config, err)
	}
}

func TestCollectorTLSConfigRejectsInvalidCA(t *testing.T) {
	certFile, keyFile, _ := testCollectorCertificates(t)
	caFile := filepath.Join(t.TempDir(), "bad-ca.pem")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectorTLSConfig(certFile, keyFile, CollectorMTLS{Mode: "require", CAFile: caFile}); err == nil {
		t.Fatal("expected invalid CA to fail")
	}
}
