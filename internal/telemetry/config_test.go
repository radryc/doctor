package telemetry

import "testing"

func TestLoadConfigDefaultsInsecureByScheme(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "")
	cfg, err := LoadConfig("doctor-test")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Insecure {
		t.Fatal("HTTPS endpoint must not default to insecure transport")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4317")
	cfg, err = LoadConfig("doctor-test")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Insecure {
		t.Fatal("host:port endpoint should default to insecure transport")
	}
}
