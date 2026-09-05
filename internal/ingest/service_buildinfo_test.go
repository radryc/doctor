package ingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rydzu/ainfra/doctor/internal/buildinfo"
)

func TestHandlerBuildInfoReturnsStampedRelease(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime
	buildinfo.Version = "20260511-0802"
	buildinfo.Commit = "fedcba9876543210"
	buildinfo.BuildTime = "2026-05-11T08:02:00Z"
	t.Cleanup(func() {
		buildinfo.Version = oldVersion
		buildinfo.Commit = oldCommit
		buildinfo.BuildTime = oldBuildTime
	})

	svc := NewServiceWithBuffers("X-Doctor-Tenant", "default", nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status/buildinfo", nil)
	rec := httptest.NewRecorder()

	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string            `json:"status"`
		Data   map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if got := resp.Data["version"]; got != "20260511-0802" {
		t.Fatalf("version = %q, want 20260511-0802", got)
	}
	if got := resp.Data["revision"]; got != "fedcba9876543210" {
		t.Fatalf("revision = %q, want fedcba9876543210", got)
	}
}
