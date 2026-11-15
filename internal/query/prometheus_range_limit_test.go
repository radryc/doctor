package query

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRangePointLimitRejectsBeforeExecution(t *testing.T) {
	svc := &Service{}
	end := time.Now().UTC()
	start := end.Add(-30 * 24 * time.Hour)
	url := "/api/v1/query_range?query=up&start=" +
		start.Format(time.RFC3339) + "&end=" + end.Format(time.RFC3339) + "&step=1s"
	rec := httptest.NewRecorder()
	svc.handlePrometheusQueryRange(rec, httptest.NewRequest("GET", url, nil))
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "maximum is") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}
