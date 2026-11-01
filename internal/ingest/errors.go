package ingest

import "errors"

// ErrBufferFull is returned by Append when the in-memory pending buffer has
// reached its MaxPendingBytes limit. Callers should treat this as a transient
// backpressure signal and retry after a brief delay.
var ErrBufferFull = errors.New("buffer full")

// ErrTenantDenied is returned when a request names a tenant outside the
// configured ingest allowlist. Collectors should not be able to write into an
// arbitrary tenant by changing a header.
var ErrTenantDenied = errors.New("tenant not allowed")

// ErrGuardianEventsUnsupported reports that the configured writer cannot store
// Guardian events. The HTTP endpoint returns 501 instead of pretending the
// events were accepted.
var ErrGuardianEventsUnsupported = errors.New("guardian event storage is not supported by this writer")
