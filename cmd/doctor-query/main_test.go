package main

import (
	"context"
	"testing"

	"github.com/radryc/doctor/internal/config"
)

func TestLoadFederationCatalogInlineJSON(t *testing.T) {
	t.Setenv("AWSREAD_TOKEN", "test-token")
	cfg := config.QueryConfig{
		FederationCatalogJSON: `{
			"version": "test",
			"targets": [{
				"id": "aws-account-a",
				"account": "111111111111",
				"region": "us-east-1",
				"partitions": ["checkout"],
				"endpoint": "http://guardian-awsread:19090",
				"token_env": "AWSREAD_TOKEN"
			}]
		}`,
	}
	catalog, source, store, err := loadFederationCatalog(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("loadFederationCatalog: %v", err)
	}
	if catalog.Len() != 1 {
		t.Fatalf("catalog length = %d, want 1", catalog.Len())
	}
	if source != "inline:test" {
		t.Fatalf("source = %q, want inline:test", source)
	}
	if store != nil {
		t.Fatalf("store = %#v, want nil for inline catalog", store)
	}
}
