package awsstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
