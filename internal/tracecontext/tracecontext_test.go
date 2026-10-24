package tracecontext

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestCanonicalTraceID(t *testing.T) {
	w3c := "4bf92f3577b34da6a3ce929d0e0e4736"
	xray := "1-4bf92f35-77b34da6a3ce929d0e0e4736"
	for _, input := range []string{w3c, xray, strings.ToUpper(w3c)} {
		got, err := CanonicalTraceID(input)
		if err != nil {
			t.Fatalf("CanonicalTraceID(%q): %v", input, err)
		}
		if got != w3c {
			t.Fatalf("CanonicalTraceID(%q) = %q, want %q", input, got, w3c)
		}
	}
	for _, input := range []string{"", "abc", strings.Repeat("0", 32), "1-xyz-77b34da6a3ce929d0e0e4736"} {
		if _, err := CanonicalTraceID(input); err == nil {
			t.Fatalf("CanonicalTraceID(%q) succeeded, want error", input)
		}
	}
}

func TestXRayTraceIDRoundTrip(t *testing.T) {
	w3c := "4bf92f3577b34da6a3ce929d0e0e4736"
	xray, err := XRayTraceID(w3c)
	if err != nil {
		t.Fatal(err)
	}
	if xray != "1-4bf92f35-77b34da6a3ce929d0e0e4736" {
		t.Fatalf("xray = %q", xray)
	}
	back, err := FromXRayTraceID(xray)
	if err != nil || back != w3c {
		t.Fatalf("round trip = %q, %v", back, err)
	}
}

func TestIsXRayCompatible(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	id := strings.ToLower(hex.EncodeToString([]byte{
		byte(now.Unix() >> 24), byte(now.Unix() >> 16), byte(now.Unix() >> 8), byte(now.Unix()),
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12,
	}))
	if !IsXRayCompatible(id, now) {
		t.Fatalf("expected %q to be compatible", id)
	}
	old := "00000001" + strings.Repeat("a", 24)
	if IsXRayCompatible(old, now) {
		t.Fatalf("expected old ID to be incompatible")
	}
}

func TestXRayIDGenerator(t *testing.T) {
	generator := XRayIDGenerator()
	now := time.Now().UTC()
	seen := make(map[string]struct{})
	for i := 0; i < 32; i++ {
		traceID, spanID := generator.NewIDs(context.Background())
		canonical, err := CanonicalTraceID(traceID.String())
		if err != nil {
			t.Fatalf("generated trace ID %q: %v", traceID, err)
		}
		if !IsXRayCompatible(canonical, now) {
			t.Fatalf("generated trace ID %q is not X-Ray compatible", canonical)
		}
		if _, ok := seen[canonical]; ok {
			t.Fatalf("duplicate trace ID %q", canonical)
		}
		seen[canonical] = struct{}{}
		if spanID.String() == "" {
			t.Fatal("empty span ID")
		}
	}
}

func TestBridgePropagatorW3CFirst(t *testing.T) {
	w3cID := "4bf92f3577b34da6a3ce929d0e0e4736"
	xrayID := "1-11111111-222222222222222222222222"
	carrier := propagation.MapCarrier{
		"traceparent":     "00-" + w3cID + "-00f067aa0ba902b7-01",
		"X-Amzn-Trace-Id": "Root=" + xrayID + ";Parent=1111111111111111;Sampled=1",
	}
	ctx := NewBridgePropagator().Extract(context.Background(), carrier)
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		t.Fatal("expected valid span context")
	}
	if got := spanContext.TraceID().String(); got != w3cID {
		t.Fatalf("trace ID = %q, want W3C %q", got, w3cID)
	}
}

func TestBridgePropagatorFallsBackToXRay(t *testing.T) {
	xrayID := "1-4bf92f35-77b34da6a3ce929d0e0e4736"
	carrier := propagation.MapCarrier{
		"X-Amzn-Trace-Id": "Root=" + xrayID + ";Parent=00f067aa0ba902b7;Sampled=1",
	}
	ctx := NewBridgePropagator().Extract(context.Background(), carrier)
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		t.Fatal("expected valid span context")
	}
	if got := spanContext.TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace ID = %q", got)
	}
}

func TestBridgePropagatorInjectsBoth(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	carrier := propagation.MapCarrier{}
	NewBridgePropagator().Inject(ctx, carrier)
	if !strings.Contains(carrier.Get("traceparent"), "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("traceparent = %q", carrier.Get("traceparent"))
	}
	if !strings.Contains(carrier.Get("X-Amzn-Trace-Id"), "Root=1-4bf92f35-77b34da6a3ce929d0e0e4736") {
		t.Fatalf("X-Amzn-Trace-Id = %q", carrier.Get("X-Amzn-Trace-Id"))
	}
}
