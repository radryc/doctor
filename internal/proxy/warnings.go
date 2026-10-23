package proxy

import (
	"context"
	"sync"
)

const maxCollectedWarnings = 64

// WarningCollector accumulates partial-failure details for one request.
// Warnings are capped to keep headers bounded; Total still counts every event.
type WarningCollector struct {
	mu       sync.Mutex
	warnings []Warning
	seen     map[string]struct{}
	total    int
}

type warningCollectorKey struct{}

// NewWarningCollector builds an empty collector.
func NewWarningCollector() *WarningCollector {
	return &WarningCollector{seen: make(map[string]struct{})}
}

// WithWarningCollector attaches a collector to a request context.
func WithWarningCollector(ctx context.Context) (context.Context, *WarningCollector) {
	collector := NewWarningCollector()
	return context.WithValue(ctx, warningCollectorKey{}, collector), collector
}

// WarningCollectorFromContext returns the request collector, if any.
func WarningCollectorFromContext(ctx context.Context) *WarningCollector {
	if ctx == nil {
		return nil
	}
	collector, _ := ctx.Value(warningCollectorKey{}).(*WarningCollector)
	return collector
}

// AddWarning records one partial failure on the request context.
func AddWarning(ctx context.Context, warning Warning) {
	collector := WarningCollectorFromContext(ctx)
	if collector == nil {
		return
	}
	collector.Add(warning)
}

// Add records one warning, deduplicating identical backend/signal/message
// triples produced by retries and trace hydration.
func (c *WarningCollector) Add(warning Warning) {
	if c == nil {
		return
	}
	key := warning.Backend + "\x00" + string(warning.Signal) + "\x00" + warning.Message
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	if _, ok := c.seen[key]; ok {
		return
	}
	if len(c.warnings) >= maxCollectedWarnings {
		return
	}
	c.seen[key] = struct{}{}
	c.warnings = append(c.warnings, warning)
}

// Warnings returns a copy of the capped warning list.
func (c *WarningCollector) Warnings() []Warning {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Warning(nil), c.warnings...)
}

// Total reports the number of warning events, including deduplicated ones.
func (c *WarningCollector) Total() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// WarningsFromContext returns a copy of the request warnings.
func WarningsFromContext(ctx context.Context) []Warning {
	return WarningCollectorFromContext(ctx).Warnings()
}

// WarningTotalFromContext returns the request warning event count.
func WarningTotalFromContext(ctx context.Context) int {
	return WarningCollectorFromContext(ctx).Total()
}
