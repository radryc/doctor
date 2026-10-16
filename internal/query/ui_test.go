package query

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleUIFallbackNeverServesHTMLToAPIClients pins the contract that broke
// Grafana: an unregistered API route must produce a 404 the client can decode,
// never a 200 with the SPA shell.
func TestHandleUIFallbackNeverServesHTMLToAPIClients(t *testing.T) {
	svc := &Service{}

	cases := []struct {
		name       string
		path       string
		accept     string
		wantStatus int
		wantHTML   bool
	}{
		{
			name:       "loki index stats (loki prefix, JSON client)",
			path:       "/loki/api/v1/index/stats",
			accept:     "application/json",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "loki volume range (loki prefix, wildcard client)",
			path:       "/loki/api/v1/index/volume_range",
			accept:     "*/*",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "tempo metrics (api prefix, wildcard client)",
			path:       "/api/metrics/query_range",
			accept:     "*/*",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown API path (v1 prefix)",
			path:       "/v1/does/not/exist",
			accept:     "application/json",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown non-prefixed path for JSON client",
			path:       "/totally/unknown",
			accept:     "application/json",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "browser deep link falls back to SPA shell",
			path:       "/partition/edge",
			accept:     "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			wantStatus: http.StatusOK,
			wantHTML:   true,
		},
		{
			name:       "root always serves the shell",
			path:       "/",
			accept:     "text/html",
			wantStatus: http.StatusOK,
			wantHTML:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := httptest.NewRecorder()
			svc.handleUI(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			hasHTML := strings.Contains(strings.ToLower(body), "<!doctype html")
			if hasHTML != tc.wantHTML {
				t.Fatalf("html body = %v, want %v (body %q)", hasHTML, tc.wantHTML, body)
			}
		})
	}
}
