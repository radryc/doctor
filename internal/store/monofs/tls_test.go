package monofs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTLSConfigDefaultsToPlaintext(t *testing.T) {
	for _, key := range []string{
		"DOCTOR_MONOFS_TLS_CA",
		"DOCTOR_MONOFS_TLS_CERT",
		"DOCTOR_MONOFS_TLS_KEY",
		"DOCTOR_MONOFS_TLS_SERVER_NAME",
		"DOCTOR_MONOFS_TLS_REQUIRE",
		"DOCTOR_MONOFS_TLS_INSECURE",
		"DOCTOR_MONOFS_TOKEN",
	} {
		t.Setenv(key, "")
	}
	creds, err := TLSConfigFromEnv().Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().SecurityProtocol; got != "insecure" {
		t.Fatalf("default protocol = %q, want insecure", got)
	}
}

func TestTLSConfigRequireUsesTLS(t *testing.T) {
	t.Setenv("DOCTOR_MONOFS_TLS_REQUIRE", "true")
	creds, err := TLSConfigFromEnv().Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Fatalf("required protocol = %q, want tls", got)
	}
}

func TestTLSConfigRejectsInsecureAndRequire(t *testing.T) {
	t.Setenv("DOCTOR_MONOFS_TLS_REQUIRE", "true")
	t.Setenv("DOCTOR_MONOFS_TLS_INSECURE", "true")
	if _, err := TLSConfigFromEnv().Credentials(); err == nil {
		t.Fatal("expected insecure+require to fail")
	}
}

func TestTLSConfigRejectsPartialClientCert(t *testing.T) {
	t.Setenv("DOCTOR_MONOFS_TLS_CERT", "/tmp/cert.pem")
	if _, err := TLSConfigFromEnv().Credentials(); err == nil {
		t.Fatal("expected partial client certificate to fail")
	}
}

func TestTLSConfigLoadsCA(t *testing.T) {
	certPath := filepath.Join(t.TempDir(), "ca.pem")
	// An invalid bundle must fail closed.
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_MONOFS_TLS_CA", certPath)
	if _, err := TLSConfigFromEnv().Credentials(); err == nil {
		t.Fatal("expected invalid CA bundle to fail")
	}
}

func TestBearerTokenRequireTransportSecurity(t *testing.T) {
	if (bearerToken{token: "t", requireTLS: false}).RequireTransportSecurity() {
		t.Fatal("RequireTransportSecurity() = true, want false when requireTLS=false")
	}
	if !(bearerToken{token: "t", requireTLS: true}).RequireTransportSecurity() {
		t.Fatal("RequireTransportSecurity() = false, want true when requireTLS=true")
	}
}

func TestTLSConfigRequiresTLSWhenTokenConfigured(t *testing.T) {
	for _, key := range []string{
		"DOCTOR_MONOFS_TLS_CA",
		"DOCTOR_MONOFS_TLS_CERT",
		"DOCTOR_MONOFS_TLS_KEY",
		"DOCTOR_MONOFS_TLS_SERVER_NAME",
		"DOCTOR_MONOFS_TLS_REQUIRE",
		"DOCTOR_MONOFS_TLS_INSECURE",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("DOCTOR_MONOFS_TOKEN", "service-token")

	creds, err := TLSConfigFromEnv().Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Fatalf("protocol with token = %q, want tls", got)
	}
}

func TestTLSConfigExplicitLocalOptOut(t *testing.T) {
	t.Setenv("DOCTOR_MONOFS_TOKEN", "service-token")
	t.Setenv("DOCTOR_MONOFS_TLS_REQUIRE", "false")
	creds, err := TLSConfigFromEnv().Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().SecurityProtocol; got != "insecure" {
		t.Fatalf("explicit local protocol = %q, want insecure", got)
	}
}

func TestEnsureBearerTransportRejectsSilentPlaintext(t *testing.T) {
	t.Setenv("DOCTOR_MONOFS_TOKEN", "")
	t.Setenv("DOCTOR_MONOFS_TLS_REQUIRE", "")
	t.Setenv("DOCTOR_MONOFS_TLS_INSECURE", "")
	creds, err := TLSConfigFromEnv().Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureBearerTransport("explicit-token", creds); err == nil {
		t.Fatal("expected bearer over implicit plaintext to fail")
	}

	t.Setenv("DOCTOR_MONOFS_TLS_INSECURE", "true")
	creds, err = TLSConfigFromEnv().Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureBearerTransport("explicit-token", creds); err != nil {
		t.Fatalf("explicit insecure opt-out rejected: %v", err)
	}

	t.Setenv("DOCTOR_MONOFS_TLS_INSECURE", "")
	t.Setenv("DOCTOR_MONOFS_TLS_REQUIRE", "false")
	creds, err = TLSConfigFromEnv().Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureBearerTransport("explicit-token", creds); err != nil {
		t.Fatalf("explicit local TLS opt-out rejected: %v", err)
	}
}
