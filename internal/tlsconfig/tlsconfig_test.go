package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/radryc/tlsx"
	"github.com/radryc/tlsx/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const testTrustDomain = "doctor.test"

type testPKI struct {
	dir      string
	caFile   string
	server   pki.ServiceBundle
	client   pki.ServiceBundle
	clientCA string
}

func generateTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	bundle, err := pki.Generate(pki.Options{
		Dir:         dir,
		TrustDomain: testTrustDomain,
		Services: []pki.Service{
			{
				Name:      "doctor-server",
				Namespace: "doctor",
				DNSNames:  []string{"localhost", "doctor-server"},
				IPs:       []string{"127.0.0.1"},
			},
			{
				Name:      "doctor-client",
				Namespace: "doctor",
				DNSNames:  []string{"localhost", "doctor-client"},
				IPs:       []string{"127.0.0.1"},
			},
		},
	})
	if err != nil {
		t.Fatalf("generate test PKI: %v", err)
	}
	return testPKI{
		dir:      dir,
		caFile:   bundle.CAFile,
		server:   bundle.Services["doctor-server"],
		client:   bundle.Services["doctor-client"],
		clientCA: bundle.CAFile,
	}
}

func setStrataTLSEnv(t *testing.T, mode string, p testPKI) {
	t.Helper()
	keys := []string{
		"STRATA_TLS_MODE",
		"STRATA_TLS_CERT_FILE",
		"STRATA_TLS_KEY_FILE",
		"STRATA_TLS_CA_FILE",
		"STRATA_TLS_SERVER_NAME",
		"STRATA_TLS_REQUIRE_CLIENT_CERT",
		"STRATA_TLS_ALLOWED_SPIFFE_IDS",
		"STRATA_TLS_EXPECTED_SPIFFE_IDS",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
	t.Setenv("STRATA_TLS_MODE", mode)
	if p.server.CertFile != "" {
		t.Setenv("STRATA_TLS_CERT_FILE", p.server.CertFile)
		t.Setenv("STRATA_TLS_KEY_FILE", p.server.KeyFile)
		t.Setenv("STRATA_TLS_CA_FILE", p.caFile)
		t.Setenv("STRATA_TLS_REQUIRE_CLIENT_CERT", "true")
	}
}

func TestOffKeepsLegacyBehaviour(t *testing.T) {
	setStrataTLSEnv(t, "off", testPKI{})

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled() || cfg.Required() {
		t.Fatalf("mode off reported enabled=%v required=%v", cfg.Enabled(), cfg.Required())
	}
	serverTLS, err := cfg.ServerTLS("", "")
	if err != nil {
		t.Fatal(err)
	}
	if serverTLS != nil {
		t.Fatal("mode off without legacy certificates must stay plaintext")
	}
	if clientTLS, err := cfg.ClientTLS(); err != nil || clientTLS != nil {
		t.Fatalf("mode off client TLS = %v, %v; want nil, nil", clientTLS, err)
	}
	if transport := cfg.Transport(nil); transport != nil {
		t.Fatal("mode off must not replace the default HTTP transport")
	}
	if client := cfg.HTTPClient(time.Second); client.Transport != nil {
		t.Fatal("mode off HTTP client must use the default transport")
	}
	if got := cfg.URL("http://doctor-query:18080"); got != "http://doctor-query:18080" {
		t.Fatalf("mode off rewrote URL to %q", got)
	}
}

func TestOffStillHonoursLegacyCertificatePair(t *testing.T) {
	p := generateTestPKI(t)
	setStrataTLSEnv(t, "off", testPKI{})

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := cfg.ServerTLS(p.server.CertFile, p.server.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if serverTLS == nil {
		t.Fatal("legacy certificate pair must still enable TLS")
	}
	if serverTLS.ClientAuth != tls.NoClientCert {
		t.Fatalf("legacy pair client auth = %v, want NoClientCert", serverTLS.ClientAuth)
	}
	if _, err := cfg.ServerTLS(p.server.CertFile, ""); err == nil {
		t.Fatal("partial legacy pair must fail")
	}
}

func TestRequiredServerEnforcesMTLS(t *testing.T) {
	serverPKI := generateTestPKI(t)
	setStrataTLSEnv(t, "required", serverPKI)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Required() {
		t.Fatal("expected required mode")
	}
	if got := cfg.URL("http://doctor-query:18080"); got != "https://doctor-query:18080" {
		t.Fatalf("required mode URL = %q, want https", got)
	}

	serverTLS, err := cfg.ServerTLS("", "")
	if err != nil {
		t.Fatal(err)
	}
	if serverTLS.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("client auth = %v, want RequireAndVerifyClientCert", serverTLS.ClientAuth)
	}

	addr := startHTTPSServer(t, serverTLS)

	// mTLS client succeeds.
	clientTLS, err := cfg.ClientTLS()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}, Timeout: 5 * time.Second}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("mTLS request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mTLS status = %d", resp.StatusCode)
	}

	// Plaintext fails against a TLS listener. The Go HTTP server may answer the
	// misdirected request with a plaintext 400, but it must never serve 200.
	plain := &http.Client{Timeout: 5 * time.Second}
	if plainResp, plainErr := plain.Get("http://" + addr + "/"); plainErr == nil {
		plainResp.Body.Close()
		if plainResp.StatusCode == http.StatusOK {
			t.Fatal("plaintext request unexpectedly succeeded against TLS listener")
		}
	}

	// A CA that did not sign the server fails verification.
	other := generateTestPKI(t)
	untrusted, err := tlsx.NewClientTLSConfig(tlsx.ClientConfig{
		CertFile: serverPKI.client.CertFile,
		KeyFile:  serverPKI.client.KeyFile,
		CAFile:   other.caFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	untrustedClient := &http.Client{Transport: &http.Transport{TLSClientConfig: untrusted}, Timeout: 5 * time.Second}
	if _, err := untrustedClient.Get("https://" + addr + "/"); err == nil {
		t.Fatal("request with an untrusted CA unexpectedly succeeded")
	}

	// One-way client (no certificate) is rejected by the mTLS server.
	oneWay, err := tlsx.NewClientTLSConfig(tlsx.ClientConfig{CAFile: serverPKI.caFile})
	if err != nil {
		t.Fatal(err)
	}
	oneWayClient := &http.Client{Transport: &http.Transport{TLSClientConfig: oneWay}, Timeout: 5 * time.Second}
	if _, err := oneWayClient.Get("https://" + addr + "/"); err == nil {
		t.Fatal("client without a certificate unexpectedly succeeded against mTLS server")
	}
}

func TestRequiredGRPCServerEnforcesMTLS(t *testing.T) {
	serverPKI := generateTestPKI(t)
	setStrataTLSEnv(t, "required", serverPKI)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	serverCreds, err := tlsx.ServerCredentials(cfg.Env.ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	addr := startGRPCServer(t, serverCreds)

	clientCreds, err := tlsx.ClientCredentials(cfg.Env.ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	checkGRPCHealth(t, conn)

	plainConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer plainConn.Close()
	if err := grpcHealth(plainConn); err == nil {
		t.Fatal("plaintext gRPC call unexpectedly succeeded against TLS listener")
	}

	other := generateTestPKI(t)
	untrustedCreds := credentials.NewTLS(&tls.Config{
		RootCAs:    caPool(t, other.caFile),
		MinVersion: tls.VersionTLS12,
	})
	untrustedConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(untrustedCreds))
	if err != nil {
		t.Fatal(err)
	}
	defer untrustedConn.Close()
	if err := grpcHealth(untrustedConn); err == nil {
		t.Fatal("gRPC call with an untrusted CA unexpectedly succeeded")
	}
}

func TestOptionalFallsBackToOneWayWithoutClientCA(t *testing.T) {
	p := generateTestPKI(t)
	setStrataTLSEnv(t, "optional", p)
	t.Setenv("STRATA_TLS_CA_FILE", p.dir+"/does-not-exist/ca.crt")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := cfg.ServerTLS("", "")
	if err != nil {
		t.Fatal(err)
	}
	if serverTLS.ClientAuth != tls.NoClientCert {
		t.Fatalf("optional mode client auth = %v, want NoClientCert without a readable CA", serverTLS.ClientAuth)
	}
}

func TestRequiredRejectsMissingCertificate(t *testing.T) {
	setStrataTLSEnv(t, "required", testPKI{})
	t.Setenv("STRATA_TLS_CERT_FILE", "/nonexistent/tls.crt")
	t.Setenv("STRATA_TLS_KEY_FILE", "/nonexistent/tls.key")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ServerTLS("", ""); err == nil {
		t.Fatal("required mode with an unreadable certificate must fail")
	}
}

func TestInvalidModeFails(t *testing.T) {
	for _, key := range []string{"STRATA_TLS_CERT_FILE", "STRATA_TLS_KEY_FILE", "STRATA_TLS_CA_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("STRATA_TLS_MODE", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("invalid STRATA_TLS_MODE must fail")
	}
}

func startHTTPSServer(t *testing.T, serverTLS *tls.Config) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		TLSConfig:         serverTLS,
		ReadHeaderTimeout: time.Second,
	}
	go func() {
		_ = server.Serve(tls.NewListener(listener, serverTLS))
	}()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

func startGRPCServer(t *testing.T, creds credentials.TransportCredentials) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(creds))
	health := health.NewServer()
	health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, health)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func checkGRPCHealth(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()
	if err := grpcHealth(conn); err != nil {
		t.Fatalf("gRPC health check failed: %v", err)
	}
}

func grpcHealth(conn *grpc.ClientConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	return err
}

func caPool(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		t.Fatalf("no certificates in %s", path)
	}
	return pool
}
