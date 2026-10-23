package federation

import (
	"context"
	"testing"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
)

func TestTargetsPagePaginatesAndFilters(t *testing.T) {
	catalog, err := NewCatalog([]Target{
		{ID: "aws-a", Account: "111", Region: "us-east-1", Partitions: []string{"div-a"}, Endpoint: "http://a", Token: "a"},
		{ID: "aws-b", Account: "222", Region: "us-east-1", Partitions: []string{"div-b"}, Endpoint: "http://b", Token: "b"},
		{ID: "aws-c", Account: "333", Region: "us-west-2", Partitions: []string{"div-c"}, Endpoint: "http://c", Token: "c"},
	}, "test")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	manager := NewManager(catalog, Options{ClientFactory: func(Target) (proxy.AWSClient, error) { return &fakeAWSClient{}, nil }})

	page := manager.TargetsPage(context.Background(), "", "", "", "", "", 2)
	if page.Total != 3 || page.Matched != 3 || len(page.Targets) != 2 || !page.HasMore {
		t.Fatalf("page = %#v", page)
	}
	if page.NextCursor != "aws-b" {
		t.Fatalf("next cursor = %q, want aws-b", page.NextCursor)
	}
	next := manager.TargetsPage(context.Background(), "", "", "", "", page.NextCursor, 2)
	if len(next.Targets) != 1 || next.Targets[0]["id"] != "aws-c" || next.HasMore {
		t.Fatalf("next page = %#v", next)
	}
	filtered := manager.TargetsPage(context.Background(), "", "us-east-1", "div-b", "", "", 10)
	if filtered.Matched != 1 || filtered.Targets[0]["id"] != "aws-b" {
		t.Fatalf("filtered = %#v", filtered)
	}
}
