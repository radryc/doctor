package federation

import (
	"context"
	"testing"
)

func TestMultiMergesProviders(t *testing.T) {
	awsCatalog, err := NewCatalog([]Target{{
		ID: "aws-a", Provider: "aws", Account: "123", Region: "eu-west-1",
		Endpoint: "http://aws", Token: "t", Partitions: []string{"payments"},
	}}, "v1")
	if err != nil {
		t.Fatalf("aws catalog: %v", err)
	}
	gcpCatalog, err := NewCatalog([]Target{{
		ID: "gcp-b", Provider: "gcp", Account: "demo-project", Region: "europe-west1",
		Endpoint: "http://gcp", Token: "t", Partitions: []string{"api"},
	}}, "v2")
	if err != nil {
		t.Fatalf("gcp catalog: %v", err)
	}
	awsManager := NewManager(awsCatalog, Options{})
	gcpManager := NewManager(gcpCatalog, Options{})
	multi := NewMulti(awsManager, awsManager, gcpManager)

	status := multi.Status()
	if status["targets"] != 2 {
		t.Fatalf("targets = %v", status["targets"])
	}
	if status["enabled"] != true {
		t.Fatalf("enabled = %v", status["enabled"])
	}
	accounts, _ := status["accounts"].(map[string]int)
	if accounts["123"] != 1 || accounts["demo-project"] != 1 {
		t.Fatalf("accounts = %v", accounts)
	}

	page := multi.TargetsPage(context.Background(), "", "", "", "", "", 10)
	if len(page.Targets) != 2 || page.Total != 2 {
		t.Fatalf("page = %+v", page)
	}
	if got := page.Targets[0]["id"]; got != "aws-a" {
		t.Fatalf("first target = %v", got)
	}

	page = multi.TargetsPage(context.Background(), "", "", "", "", "", 1)
	if len(page.Targets) != 1 || !page.HasMore || page.NextCursor != "aws-a" {
		t.Fatalf("paginated page = %+v", page)
	}

	if !multi.Enabled() {
		t.Fatal("expected the multi facade to be enabled")
	}
}
