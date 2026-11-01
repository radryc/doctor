package ingest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestServiceRejectsDisallowedTenant(t *testing.T) {
	svc := NewServiceWithBuffers("X-Doctor-Tenant", "default", nil, nil, nil, nil, nil, nil)
	svc.SetAllowedTenants([]string{"default"})

	err := svc.ingestTraces(context.Background(), "other", &coltracepb.ExportTraceServiceRequest{})
	if !errors.Is(err, ErrTenantDenied) {
		t.Fatalf("ingestTraces error = %v, want ErrTenantDenied", err)
	}
	if got := status.Code(grpcIngestError(err)); got != codes.PermissionDenied {
		t.Fatalf("grpc code = %s, want PermissionDenied", got)
	}
	if got := httpIngestStatus(err); got != http.StatusForbidden {
		t.Fatalf("http status = %d, want 403", got)
	}
}

func TestServiceAllowsConfiguredTenant(t *testing.T) {
	svc := NewServiceWithBuffers("X-Doctor-Tenant", "default", nil, nil, nil, nil, nil, nil)
	svc.SetAllowedTenants([]string{"default", "edge"})
	if !svc.tenantAllowed("edge") {
		t.Fatal("edge tenant should be allowed")
	}
	if svc.tenantAllowed("other") {
		t.Fatal("other tenant should be denied")
	}
	// An empty allowlist preserves the historical (unrestricted) behavior.
	svc.SetAllowedTenants(nil)
	if !svc.tenantAllowed("other") {
		t.Fatal("empty allowlist should not restrict tenants")
	}
}
