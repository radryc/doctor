package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/rydzu/ainfra/doctor/internal/access"
)

func accessContext(subject string) context.Context {
	return authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: subject})
}

func TestRouterLogsEnforcementExcludesDeniedBackends(t *testing.T) {
	local := &fakeLogsUpstream{name: "doctor"}
	loki := &fakeLogsUpstream{name: "loki-a"}
	gate := access.NewGate(access.NewStatic("", "", ""), map[access.Signal]bool{access.SignalLogs: true}, nil, nil)
	router := NewRouter(RouterConfig{MaxInflight: 4, BackendTimeout: time.Second, Access: gate}, NewCompositeRegistry())
	for name, upstream := range map[string]LogsUpstream{"doctor": local, "loki-a": loki} {
		backend := Backend{Name: name, Kind: KindMonoFS, Signals: []Signal{SignalLogs}, Policy: PolicyAlways}
		if name == "loki-a" {
			backend.Kind = KindLoki
			backend.BaseURL = "http://loki-a"
		}
		if err := router.Register(backend, Upstreams{Logs: upstream}); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	_, _, err := router.Logs(accessContext("alice"), "default", LogPushdown{Selector: `{service="api"}`}, time.Time{}, time.Now(), 10)
	if !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
}

func TestRouterLogsObserveModeKeepsBackendsAndWarns(t *testing.T) {
	local := &fakeLogsUpstream{name: "doctor", records: nil}
	gate := access.NewGate(access.NewStatic("", "", ""), nil, nil, nil)
	router := NewRouter(RouterConfig{MaxInflight: 4, BackendTimeout: time.Second, Access: gate}, NewCompositeRegistry())
	if err := router.Register(Backend{Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalLogs}, Policy: PolicyAlways}, Upstreams{Logs: local}); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, warnings, err := router.Logs(accessContext("alice"), "default", LogPushdown{Selector: `{service="api"}`}, time.Time{}, time.Now(), 10)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Backend != "doctor" {
		t.Fatalf("warnings = %#v, want one observed denial", warnings)
	}
	if local.calls == 0 {
		t.Fatal("observe mode must still query the backend")
	}
}

func TestRouterLogsAllowedBackendPasses(t *testing.T) {
	local := &fakeLogsUpstream{name: "doctor"}
	gate := access.NewGate(access.NewStatic("alice", "", ""), map[access.Signal]bool{access.SignalLogs: true}, nil, nil)
	router := NewRouter(RouterConfig{MaxInflight: 4, BackendTimeout: time.Second, Access: gate}, NewCompositeRegistry())
	if err := router.Register(Backend{Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalLogs}, Policy: PolicyAlways}, Upstreams{Logs: local}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := router.Logs(accessContext("alice"), "default", LogPushdown{Selector: `{service="api"}`}, time.Time{}, time.Now(), 10); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if local.calls != 1 {
		t.Fatalf("calls = %d, want 1", local.calls)
	}
}

func TestRouterEndpointsReportsDecisions(t *testing.T) {
	gate := access.NewGate(access.NewStatic("", "", ""), nil, nil, nil)
	router := NewRouter(RouterConfig{Access: gate}, NewCompositeRegistry())
	if err := router.Register(Backend{Name: "loki-a", Kind: KindLoki, BaseURL: "http://loki-a", Signals: []Signal{SignalLogs}}, Upstreams{Logs: &fakeLogsUpstream{name: "loki-a"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	endpoints := router.Endpoints(accessContext("alice"))
	if len(endpoints) != 1 || endpoints[0].Allowed {
		t.Fatalf("endpoints = %#v, want denied", endpoints)
	}
	if endpoints[0].Resolver != "static" {
		t.Fatalf("resolver = %q, want static", endpoints[0].Resolver)
	}
}
