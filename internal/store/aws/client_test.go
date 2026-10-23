package awsstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestListLogGroupsAcceptsArrayAndObjectResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "bare array", body: `["/aws/lambda/a","/ecs/b"]`},
		{name: "object", body: `{"log_groups":["/aws/lambda/a","/ecs/b"]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/aws/log-groups" {
					t.Fatalf("path = %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			client := NewClient(server.URL, "")
			groups, err := client.ListLogGroups(context.Background())
			if err != nil {
				t.Fatalf("ListLogGroups() error = %v", err)
			}
			if len(groups) != 2 || groups[0] != "/aws/lambda/a" || groups[1] != "/ecs/b" {
				t.Fatalf("ListLogGroups() = %#v", groups)
			}
		})
	}
}

type testTokenSource struct {
	mu          sync.Mutex
	token       string
	refreshed   string
	invalidated int
}

func (s *testTokenSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalidated > 0 && s.refreshed != "" {
		return s.refreshed, nil
	}
	return s.token, nil
}

func (s *testTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
}

func TestTokenSourceRefreshesOn401(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer new-token" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`["/aws/test"]`))
	}))
	defer server.Close()

	source := &testTokenSource{token: "old-token", refreshed: "new-token"}
	client := NewClientWithTokenSource(server.URL, source)
	groups, err := client.ListLogGroups(context.Background())
	if err != nil {
		t.Fatalf("ListLogGroups() error = %v", err)
	}
	if len(groups) != 1 || groups[0] != "/aws/test" {
		t.Fatalf("groups = %#v", groups)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if source.invalidated != 1 {
		t.Fatalf("invalidations = %d, want 1", source.invalidated)
	}
}

func TestPromQLRefreshesTokenAndRetriesPostBody(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer new-token" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	source := &testTokenSource{token: "old-token", refreshed: "new-token"}
	client := NewClientWithTokenSource(server.URL, source)
	resp, err := client.PrometheusQuery(context.Background(), "up", time.Now())
	if err != nil {
		t.Fatalf("PrometheusQuery() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if resp["status"] != "success" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestHTTPErrorKeepsStatusCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer server.Close()
	client := NewClient(server.URL, "")
	_, err := client.ListLogGroups(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusForbidden {
		t.Fatalf("error = %v, want HTTPError 403", err)
	}
}
