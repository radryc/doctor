package federation

import (
	"context"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

func TestManagerListServicesUsesFreshEmptyMetadata(t *testing.T) {
	catalog, err := NewCatalog([]Target{{
		ID:                "aws-a",
		Account:           "111111111111",
		Region:            "us-east-1",
		Endpoint:          "http://a",
		Token:             "a",
		MetadataFetchedAt: time.Now().UTC(),
	}}, "fresh")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	client := &fakeAWSClient{services: []awsstore.ServiceInfo{{Name: "should-not-be-called"}}}
	manager := NewManager(catalog, Options{
		MetadataTTL: time.Hour,
		ClientFactory: func(Target) (proxy.AWSClient, error) {
			return client, nil
		},
	})
	services, err := manager.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(services) != 0 {
		t.Fatalf("services = %#v, want empty cached result", services)
	}
	if client.CallCount() != 0 {
		t.Fatalf("fresh empty metadata queried pusher %d times", client.CallCount())
	}
}

func TestManagerListServicesCachesFallbackResults(t *testing.T) {
	catalog, err := NewCatalog([]Target{{
		ID:       "aws-a",
		Account:  "111111111111",
		Region:   "us-east-1",
		Endpoint: "http://a",
		Token:    "a",
	}}, "stale")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	client := &fakeAWSClient{services: []awsstore.ServiceInfo{{Name: "api", Account: "111111111111", Region: "us-east-1"}}}
	manager := NewManager(catalog, Options{
		MetadataTTL: time.Hour,
		ClientFactory: func(Target) (proxy.AWSClient, error) {
			return client, nil
		},
	})
	for i := 0; i < 2; i++ {
		services, err := manager.ListServices(context.Background())
		if err != nil || len(services) != 1 || services[0].Name != "api" {
			t.Fatalf("ListServices[%d] = %#v, %v", i, services, err)
		}
	}
	if client.CallCount() != 1 {
		t.Fatalf("fallback calls = %d, want 1 cached call", client.CallCount())
	}
}

func TestManagerListServicesCachesEmptyFallback(t *testing.T) {
	catalog, err := NewCatalog([]Target{{
		ID:       "aws-a",
		Account:  "111111111111",
		Region:   "us-east-1",
		Endpoint: "http://a",
		Token:    "a",
	}}, "stale-empty")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	client := &fakeAWSClient{services: []awsstore.ServiceInfo{}}
	manager := NewManager(catalog, Options{
		MetadataTTL: time.Hour,
		ClientFactory: func(Target) (proxy.AWSClient, error) {
			return client, nil
		},
	})
	for i := 0; i < 2; i++ {
		services, err := manager.ListServices(context.Background())
		if err != nil || len(services) != 0 {
			t.Fatalf("ListServices[%d] = %#v, %v", i, services, err)
		}
	}
	if client.CallCount() != 1 {
		t.Fatalf("empty fallback calls = %d, want 1", client.CallCount())
	}
}
