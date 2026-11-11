package quota

import (
	"errors"
	"fmt"
	"time"
)

// UnknownPartition is the bucket used for records without a Guardian
// partition, and the partition name rejected in enforce mode when it is not in
// the catalog.
const UnknownPartition = "unknown"

var (
	// ErrQuotaExceeded is returned when admission is rejected.
	ErrQuotaExceeded = errors.New("quota exceeded")
	// ErrUnknownPartition is returned for telemetry that does not name a known
	// Guardian partition while enforcement is on.
	ErrUnknownPartition = errors.New("unknown partition")
)

// LimitError describes the bucket that rejected a request.
type LimitError struct {
	Key        string
	Resource   string
	Limit      float64
	RetryAfter time.Duration
	Observed   bool
}

func (e *LimitError) Error() string {
	if e == nil {
		return ErrQuotaExceeded.Error()
	}
	if e.Observed {
		return fmt.Sprintf("quota observed: %s %s over limit %.2f", e.Key, e.Resource, e.Limit)
	}
	return fmt.Sprintf("quota exceeded: %s %s over limit %.2f", e.Key, e.Resource, e.Limit)
}

// Is implements errors.Is for ErrQuotaExceeded.
func (e *LimitError) Is(target error) bool { return target == ErrQuotaExceeded }

// Usage is one bucket's dashboard snapshot.
type Usage struct {
	Key       string  `json:"key"`
	Scope     string  `json:"scope"`
	Resource  string  `json:"resource"`
	Limit     float64 `json:"limit"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining,omitempty"`
	Inflight  int     `json:"inflight,omitempty"`
	Accepted  int64   `json:"accepted"`
	Rejected  int64   `json:"rejected"`
}

// stat tracks counters for a bucket key.
type stat struct {
	scope    string
	resource string
	accepted int64
	rejected int64
	lastSeen time.Time
}

func (s *stat) clone(key string) Usage {
	return Usage{Key: key, Scope: s.scope, Resource: s.resource, Accepted: s.accepted, Rejected: s.rejected}
}
