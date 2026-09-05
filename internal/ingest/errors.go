package ingest

import "errors"

// ErrBufferFull is returned by Append when the in-memory pending buffer has
// reached its MaxPendingBytes limit. Callers should treat this as a transient
// backpressure signal and retry after a brief delay.
var ErrBufferFull = errors.New("buffer full")
