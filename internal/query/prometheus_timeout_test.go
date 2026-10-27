package query

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestPrometheusContext(t *testing.T) {
	tests := []struct {
		name      string
		timeout   string
		wantError bool
		min       time.Duration
		max       time.Duration
	}{
		{name: "default has no extra deadline", min: 0, max: 0},
		{name: "long query", timeout: "2m", min: 90 * time.Second, max: 2 * time.Minute},
		{name: "clamped to maximum", timeout: "10m", min: 4 * time.Minute, max: maxPrometheusQueryTimeout},
		{name: "invalid duration", timeout: "soon", wantError: true},
		{name: "zero duration", timeout: "0s", wantError: true},
		{name: "negative duration", timeout: "-30s", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := "/api/v1/query?query=up"
			if tc.timeout != "" {
				url += "&timeout=" + tc.timeout
			}
			req := httptest.NewRequest("GET", url, nil)
			ctx, cancel, err := requestPrometheusContext(req)
			if cancel != nil {
				defer cancel()
			}
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected an error for timeout %q", tc.timeout)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			deadline, ok := ctx.Deadline()
			if tc.max == 0 {
				if ok {
					t.Fatalf("unexpected deadline %v for default request", deadline)
				}
				return
			}
			if !ok {
				t.Fatal("expected a deadline")
			}
			remaining := time.Until(deadline)
			if remaining < tc.min || remaining > tc.max {
				t.Fatalf("deadline in %v, want between %v and %v", remaining, tc.min, tc.max)
			}
		})
	}
}
