package federation

import (
	"context"
	"strconv"
	"sync"
)

// RequestMeta carries query-plane federation headers from the manager back to
// the HTTP middleware. It is request-scoped and safe for trace hydration, which
// may plan more than once.
type RequestMeta struct {
	mu       sync.Mutex
	version  string
	matched  int
	selected int
}

type requestMetaKey struct{}

// WithRequestMeta attaches a federation header collector to ctx.
func WithRequestMeta(ctx context.Context) (context.Context, *RequestMeta) {
	meta := &RequestMeta{}
	return context.WithValue(ctx, requestMetaKey{}, meta), meta
}

// RequestMetaFromContext returns the request federation metadata, if any.
func RequestMetaFromContext(ctx context.Context) *RequestMeta {
	if ctx == nil {
		return nil
	}
	meta, _ := ctx.Value(requestMetaKey{}).(*RequestMeta)
	return meta
}

// SetPlan records the latest or widest plan for the request.
func (m *RequestMeta) SetPlan(version string, matched, selected int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if version != "" {
		m.version = version
	}
	if matched > m.matched {
		m.matched = matched
	}
	if selected > m.selected {
		m.selected = selected
	}
}

// HeaderValues returns the response header values, omitting empty fields.
func (m *RequestMeta) HeaderValues() (version string, matched string, selected string) {
	if m == nil {
		return "", "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.version != "" {
		version = m.version
	}
	if m.matched > 0 {
		matched = strconv.Itoa(m.matched)
	}
	if m.selected > 0 {
		selected = strconv.Itoa(m.selected)
	}
	return version, matched, selected
}
