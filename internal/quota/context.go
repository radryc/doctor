package quota

import "context"

type requestBytesKey struct{}

// WithRequestBytes records the wire size of the current OTLP request so quota
// admission can charge bytes without re-marshalling the payload.
func WithRequestBytes(ctx context.Context, size int64) context.Context {
	if size <= 0 {
		return ctx
	}
	return context.WithValue(ctx, requestBytesKey{}, size)
}

// RequestBytes returns the wire size recorded by WithRequestBytes.
func RequestBytes(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	size, _ := ctx.Value(requestBytesKey{}).(int64)
	return size
}
