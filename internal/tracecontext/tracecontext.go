// Package tracecontext contains the platform's canonical trace identity and
// wire-format bridge. Customer applications use standard OpenTelemetry; this
// package is used by Doctor and the trace backend adapters.
package tracecontext

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// TraceIDHexLength is the W3C canonical trace ID length.
	TraceIDHexLength = 32
	// SpanIDHexLength is the W3C canonical span ID length.
	SpanIDHexLength = 16
	// XRayRetention is the default X-Ray trace retention window.
	XRayRetention = 30 * 24 * time.Hour
)

var (
	// ErrInvalidTraceID reports a malformed or zero trace ID.
	ErrInvalidTraceID = errors.New("invalid trace id")
	// ErrInvalidSpanID reports a malformed or zero span ID.
	ErrInvalidSpanID = errors.New("invalid span id")
)

// CanonicalTraceID converts either a W3C 32-hex trace ID or an X-Ray
// 1-<8hex>-<24hex> trace ID to the canonical lowercase 32-hex form.
func CanonicalTraceID(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if isXRayForm(value) {
		value = value[2:10] + value[11:]
	}
	if len(value) != TraceIDHexLength || !isHex(value) || isZeroHex(value) {
		return "", ErrInvalidTraceID
	}
	return value, nil
}

// CanonicalSpanID validates and normalizes a 16-hex span ID.
func CanonicalSpanID(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if len(value) != SpanIDHexLength || !isHex(value) || isZeroHex(value) {
		return "", ErrInvalidSpanID
	}
	return value, nil
}

// XRayTraceID converts a canonical or X-Ray trace ID to X-Ray header form.
func XRayTraceID(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if isXRayForm(value) {
		return value, nil
	}
	canonical, err := CanonicalTraceID(value)
	if err != nil {
		return "", err
	}
	return "1-" + canonical[:8] + "-" + canonical[8:], nil
}

// FromXRayTraceID converts an X-Ray trace ID to canonical 32-hex form.
func FromXRayTraceID(raw string) (string, error) {
	return CanonicalTraceID(raw)
}

// IsXRayTraceID reports whether raw uses the X-Ray 1-<8hex>-<24hex> format.
func IsXRayTraceID(raw string) bool {
	return isXRayForm(strings.ToLower(strings.TrimSpace(raw)))
}

// IsXRayCompatible reports whether canonical's embedded epoch is inside the
// window accepted by X-Ray. X-Ray rejects trace IDs whose timestamp is outside
// its retention window.
func IsXRayCompatible(canonical string, now time.Time) bool {
	value, err := CanonicalTraceID(canonical)
	if err != nil {
		return false
	}
	seconds, err := hex.DecodeString(value[:8])
	if err != nil || len(seconds) != 4 {
		return false
	}
	epoch := int64(seconds[0])<<24 | int64(seconds[1])<<16 | int64(seconds[2])<<8 | int64(seconds[3])
	started := time.Unix(epoch, 0).UTC()
	return !started.Before(now.UTC().Add(-XRayRetention)) && !started.After(now.UTC().Add(5*time.Minute))
}

// XRayIDGenerator returns an OpenTelemetry ID generator that creates trace IDs
// with the X-Ray-compatible epoch prefix.
func XRayIDGenerator() sdktrace.IDGenerator {
	return xray.NewIDGenerator()
}

// BridgePropagator extracts W3C Trace Context first and falls back to AWS
// X-Ray. It injects both formats so an outbound request can cross k8s and AWS
// boundaries without starting a new trace.
type BridgePropagator struct {
	w3c  propagation.TextMapPropagator
	xray propagation.TextMapPropagator
}

// NewBridgePropagator builds a propagator that writes both W3C and X-Ray
// headers and reads W3C first. A valid W3C context wins if both headers are
// present and disagree.
func NewBridgePropagator() *BridgePropagator {
	return &BridgePropagator{
		w3c:  propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
		xray: xray.Propagator{},
	}
}

// Inject writes W3C and X-Ray trace context.
func (p *BridgePropagator) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	if p == nil {
		return
	}
	if p.w3c != nil {
		p.w3c.Inject(ctx, carrier)
	}
	if p.xray != nil {
		p.xray.Inject(ctx, carrier)
	}
}

// Extract reads W3C first and falls back to X-Ray when no valid W3C span
// context is present.
func (p *BridgePropagator) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if p == nil {
		return ctx
	}
	if p.w3c != nil {
		if extracted := p.w3c.Extract(ctx, carrier); trace.SpanContextFromContext(extracted).IsValid() {
			return extracted
		}
	}
	if p.xray != nil {
		return p.xray.Extract(ctx, carrier)
	}
	return ctx
}

// Fields returns the union of W3C, baggage, and X-Ray header names.
func (p *BridgePropagator) Fields() []string {
	if p == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 4)
	add := func(fields []string) {
		for _, field := range fields {
			if field == "" {
				continue
			}
			if _, ok := seen[field]; ok {
				continue
			}
			seen[field] = struct{}{}
			out = append(out, field)
		}
	}
	if p.w3c != nil {
		add(p.w3c.Fields())
	}
	if p.xray != nil {
		add(p.xray.Fields())
	}
	return out
}

func isXRayForm(value string) bool {
	if len(value) != 35 || value[0] != '1' || value[1] != '-' || value[10] != '-' {
		return false
	}
	return isHex(value[2:10]) && isHex(value[11:])
}

func isHex(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func isZeroHex(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] != '0' {
			return false
		}
	}
	return true
}
