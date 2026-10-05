package proxy

import "testing"

func TestBackendValidate(t *testing.T) {
	valid := Backend{Name: "prom-a", Kind: KindPrometheus, BaseURL: "http://prom-a", Signals: []Signal{SignalMetrics}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid backend rejected: %v", err)
	}

	cases := []struct {
		name    string
		backend Backend
	}{
		{"empty name", Backend{Kind: KindLoki, Signals: []Signal{SignalLogs}}},
		{"bad name", Backend{Name: "Prom A", Kind: KindLoki, Signals: []Signal{SignalLogs}}},
		{"missing kind", Backend{Name: "loki-a", Signals: []Signal{SignalLogs}}},
		{"no signals", Backend{Name: "loki-a", Kind: KindLoki}},
		{"bad signal", Backend{Name: "loki-a", Kind: KindLoki, Signals: []Signal{"events"}}},
		{"bad mode", Backend{Name: "loki-a", Kind: KindLoki, Signals: []Signal{SignalLogs}, Modes: map[Signal]Mode{SignalLogs: "nope"}}},
	}
	for _, tc := range cases {
		if err := tc.backend.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", tc.name)
		}
	}
}

func TestBackendSupportsAndMode(t *testing.T) {
	backend := Backend{
		Name:    "es-main",
		Kind:    KindElasticsearch,
		Signals: []Signal{SignalLogs, SignalTraces},
		Modes:   map[Signal]Mode{SignalLogs: ModeStore},
	}
	if !backend.Supports(SignalLogs) || backend.Supports(SignalMetrics) {
		t.Fatalf("Supports returned unexpected values")
	}
	if backend.Mode(SignalLogs) != ModeStore {
		t.Fatalf("mode = %q, want store", backend.Mode(SignalLogs))
	}
	if backend.Mode(SignalTraces) != ModeProxy {
		t.Fatalf("default mode = %q, want proxy", backend.Mode(SignalTraces))
	}
}

func TestSortBackendsByPriorityThenName(t *testing.T) {
	backends := []Backend{
		{Name: "b", Priority: 1},
		{Name: "a", Priority: 1},
		{Name: "c", Priority: 0},
	}
	SortBackends(backends)
	got := []string{backends[0].Name, backends[1].Name, backends[2].Name}
	want := []string{"c", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %#v, want %#v", got, want)
		}
	}
}
