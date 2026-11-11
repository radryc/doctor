package query

import (
	"context"
	"fmt"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/monofs/pkg/authz/policy"
)

// The access policy used to be applied only after the storage read, which made
// a per-partition user's page almost always empty: the storage returned the
// newest N spans/logs across every partition, and the policy then discarded
// nearly all of them. These optional interfaces let the policy layer push the
// caller's authorized partition set into the backend before the result limit,
// while still filtering the returned records as a second line of defence.

// scopedLogQueryBackend applies a caller's visible partitions before limiting
// a log query.
type scopedLogQueryBackend interface {
	QueryLogsScoped(ctx context.Context, tenant, query, service string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.LogRecord, error)
}

// scopedRawLogQueryBackend is the LogQL-selector form of scopedLogQueryBackend.
type scopedRawLogQueryBackend interface {
	QueryLogsRawScoped(ctx context.Context, tenant, query string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.LogRecord, error)
}

// scopedTraceQueryBackend applies a caller's visible partitions before
// limiting a trace query.
type scopedTraceQueryBackend interface {
	QueryTracesScoped(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.TraceRecord, error)
}

// tracePageQuery reports whether storage returned a full batch *before* the
// access policy filtered it, plus the oldest start time in that raw batch.
// The recent-trace scan must keep walking older batches while the raw batch
// was full even when every span was filtered out; the raw watermark lets it
// advance past the invisible region instead of stopping.
type tracePageQuery interface {
	QueryTracesPage(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, bool, time.Time, error)
}

// oldestTraceStart returns the earliest span start in the batch.
func oldestTraceStart(spans []segment.TraceRecord) time.Time {
	var oldest time.Time
	for _, span := range spans {
		if oldest.IsZero() || span.StartTime.Before(oldest) {
			oldest = span.StartTime
		}
	}
	return oldest
}

// readScope returns the raw partition labels the current caller may read.
// Observe mode must not push a scope down: it intentionally exposes all data.
func (b *policyBackend) readScope(ctx context.Context, signal policy.Signal) ([]string, bool) {
	if b == nil || b.gate == nil || !b.gate.Enforced() {
		return nil, false
	}
	return b.gate.ReadPartitions(identityFromContext(ctx), signal)
}

// queryLogsBackend executes a log read, using the scoped form when the backend
// supports it and the caller is restricted.
func queryLogsBackend(ctx context.Context, backend LogEngineBackend, tenant, query, service string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.LogRecord, error) {
	if restricted && len(visiblePartitions) == 0 {
		return nil, nil
	}
	if restricted {
		if scoped, ok := backend.(scopedLogQueryBackend); ok {
			return scoped.QueryLogsScoped(ctx, tenant, query, service, from, to, limit, visiblePartitions, true)
		}
	}
	return backend.QueryLogs(ctx, tenant, query, service, from, to, limit)
}

// queryRawLogsBackend executes a full LogQL selector, using the scoped form
// when available.
func queryRawLogsBackend(ctx context.Context, backend LogEngineBackend, tenant, query string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.LogRecord, error) {
	if restricted && len(visiblePartitions) == 0 {
		return nil, nil
	}
	if restricted {
		if scoped, ok := backend.(scopedRawLogQueryBackend); ok {
			return scoped.QueryLogsRawScoped(ctx, tenant, query, from, to, limit, visiblePartitions, true)
		}
	}
	raw, ok := backend.(rawLogQueryBackend)
	if !ok {
		return nil, fmt.Errorf("log backend does not support raw queries")
	}
	return raw.QueryLogsRaw(ctx, tenant, query, from, to, limit)
}

// queryTracesBackend executes a trace read, using the scoped form when the
// backend supports it and the caller is restricted.
func queryTracesBackend(ctx context.Context, backend LogEngineBackend, tenant, traceID, service string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.TraceRecord, error) {
	if restricted && len(visiblePartitions) == 0 {
		return nil, nil
	}
	if restricted {
		if scoped, ok := backend.(scopedTraceQueryBackend); ok {
			return scoped.QueryTracesScoped(ctx, tenant, traceID, service, from, to, limit, visiblePartitions, true)
		}
	}
	return backend.QueryTraces(ctx, tenant, traceID, service, from, to, limit)
}

// queryTracesPageBackend executes one bounded trace-scan batch and reports
// whether storage had a full batch before policy filtering, plus the raw
// batch's oldest start time.
func queryTracesPageBackend(ctx context.Context, backend LogEngineBackend, tenant, traceID, service string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.TraceRecord, bool, time.Time, error) {
	if restricted && len(visiblePartitions) == 0 {
		return nil, false, time.Time{}, nil
	}
	if restricted {
		if scoped, ok := backend.(scopedTraceQueryBackend); ok {
			spans, err := scoped.QueryTracesScoped(ctx, tenant, traceID, service, from, to, limit, visiblePartitions, true)
			return spans, len(spans) >= limit, oldestTraceStart(spans), err
		}
	}
	if page, ok := backend.(tracePageQuery); ok {
		return page.QueryTracesPage(ctx, tenant, traceID, service, from, to, limit)
	}
	spans, err := backend.QueryTraces(ctx, tenant, traceID, service, from, to, limit)
	if err != nil {
		return nil, false, time.Time{}, err
	}
	return spans, len(spans) >= limit, oldestTraceStart(spans), nil
}
