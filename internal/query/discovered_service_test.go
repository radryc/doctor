package query

import (
	"encoding/json"
	"testing"
)

func TestDiscoveredServiceEmitsNameAndCompatibilityAlias(t *testing.T) {
	data, err := json.Marshal(discoveredService{Name: "api", Service: "api"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["name"] != "api" {
		t.Fatalf("name = %v, want api", payload["name"])
	}
	if payload["service"] != "api" {
		t.Fatalf("service = %v, want api", payload["service"])
	}
}
