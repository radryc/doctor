package ingest

import (
	"errors"
	"net/http"
	"testing"

	"github.com/radryc/doctor/internal/quota"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestQuotaErrorMapping(t *testing.T) {
	limit := &quota.LimitError{Key: "partition:checkout", Resource: "traces.bytes"}
	if got := httpIngestStatus(limit); got != http.StatusTooManyRequests {
		t.Fatalf("http status = %d, want 429", got)
	}
	if got := status.Code(grpcIngestError(limit)); got != codes.ResourceExhausted {
		t.Fatalf("grpc code = %s, want ResourceExhausted", got)
	}
	if got := httpIngestStatus(quota.ErrUnknownPartition); got != http.StatusBadRequest {
		t.Fatalf("unknown partition status = %d, want 400", got)
	}
	if !errors.Is(limit, quota.ErrQuotaExceeded) {
		t.Fatal("LimitError must match ErrQuotaExceeded")
	}
}
