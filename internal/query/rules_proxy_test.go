package query

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRulesProxyReadAndEvaluateOnly(t *testing.T) {
	var seen []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"groups":[]}`)
	}))
	defer target.Close()

	svc := &Service{}
	svc.SetRulesURL(target.URL)

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/v1/rules", http.StatusOK},
		{http.MethodGet, "/v1/rules/default/shard-health", http.StatusOK},
		{http.MethodPost, "/v1/rules/evaluate?tenant=default&group=shard-health", http.StatusOK},
		{http.MethodPut, "/v1/rules/default/shard-health", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/rules/default/shard-health", http.StatusMethodNotAllowed},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("version: 1"))
		rec := httptest.NewRecorder()
		svc.handleRulesProxy(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s %s status = %d, want %d (body %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
	if len(seen) != 3 {
		t.Fatalf("forwarded requests = %v, want 3", seen)
	}
}

func TestRulesProxyUnavailableWithoutURL(t *testing.T) {
	svc := &Service{}
	rec := httptest.NewRecorder()
	svc.handleRulesProxy(rec, httptest.NewRequest(http.MethodGet, "/v1/rules", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
