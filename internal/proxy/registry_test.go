package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseBackendsValid(t *testing.T) {
	data := []byte(`
version: 1
backends:
  - name: prom-a
    kind: prometheus
    baseUrl: http://prom-a:9090/
    signals: [metrics]
    ownersPath: /doctor/v1/endpoints/prom-a
    policy: always
    timeout: 5s
  - name: loki-a
    kind: loki
    baseUrl: http://loki-a:3100
    signals: [logs]
    labels:
      region: eu
`)
	backends, err := ParseBackends(data)
	if err != nil {
		t.Fatalf("ParseBackends: %v", err)
	}
	if len(backends) != 2 {
		t.Fatalf("backends = %d, want 2", len(backends))
	}
	if backends[0].BaseURL != "http://prom-a:9090" {
		t.Fatalf("baseURL = %q, want trailing slash trimmed", backends[0].BaseURL)
	}
	if backends[0].Timeout != 5*time.Second {
		t.Fatalf("timeout = %s, want 5s", backends[0].Timeout)
	}
	if backends[0].QueryPolicy() != PolicyAlways {
		t.Fatalf("policy = %q, want always", backends[0].QueryPolicy())
	}
	if backends[1].Labels["region"] != "eu" {
		t.Fatalf("labels = %#v", backends[1].Labels)
	}
	if backends[0].Origin != "config" {
		t.Fatalf("origin = %q, want config", backends[0].Origin)
	}
}

func TestParseBackendsRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"duplicate": `
version: 1
backends:
  - {name: a, kind: prometheus, baseUrl: http://a, signals: [metrics]}
  - {name: a, kind: loki, baseUrl: http://b, signals: [logs]}
`,
		"bad version": `
version: 9
backends:
  - {name: a, kind: prometheus, baseUrl: http://a, signals: [metrics]}
`,
		"invalid backend": `
version: 1
backends:
  - {name: a, kind: prometheus, signals: [metrics]}
`,
	}
	for name, data := range cases {
		if _, err := ParseBackends([]byte(data)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestStaticRegistryReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backends.yaml")
	write := func(content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write(`
version: 1
backends:
  - {name: prom-a, kind: prometheus, baseUrl: http://a, signals: [metrics]}
`)

	registry, err := NewStaticRegistry(StaticRegistryConfig{Path: path})
	if err != nil {
		t.Fatalf("NewStaticRegistry: %v", err)
	}
	if got := len(registry.Snapshot()); got != 1 {
		t.Fatalf("snapshot = %d, want 1", got)
	}
	if registry.LastRefresh().IsZero() {
		t.Fatal("LastRefresh is zero")
	}

	write(`
version: 1
backends:
  - {name: prom-a, kind: prometheus, baseUrl: http://a, signals: [metrics]}
  - {name: loki-a, kind: loki, baseUrl: http://b, signals: [logs]}
`)
	if err := registry.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(registry.Snapshot()); got != 2 {
		t.Fatalf("snapshot after reload = %d, want 2", got)
	}

	write(`not: [valid`)
	if err := registry.Reload(); err == nil {
		t.Fatal("expected invalid reload to fail")
	}
	if got := len(registry.Snapshot()); got != 2 {
		t.Fatalf("invalid reload changed snapshot: %d", got)
	}
}

func TestCompositeRegistryFirstWins(t *testing.T) {
	first := NewSliceRegistry([]Backend{{
		Name: "prom-a", Kind: KindPrometheus, BaseURL: "http://pinned", Signals: []Signal{SignalMetrics},
	}})
	second := NewSliceRegistry([]Backend{
		{Name: "prom-a", Kind: KindPrometheus, BaseURL: "http://discovered", Signals: []Signal{SignalMetrics}},
		{Name: "loki-a", Kind: KindLoki, BaseURL: "http://loki", Signals: []Signal{SignalLogs}},
	})
	composite := NewCompositeRegistry(first, second)
	snapshot := composite.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("snapshot = %d, want 2", len(snapshot))
	}
	for _, backend := range snapshot {
		if backend.Name == "prom-a" && backend.BaseURL != "http://pinned" {
			t.Fatalf("prom-a baseURL = %q, want pinned", backend.BaseURL)
		}
	}
}
