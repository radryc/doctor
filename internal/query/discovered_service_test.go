package query

import (
	"encoding/json"
	"testing"
)

func TestDiscoveredServiceEmitsName(t *testing.T) {
	data, err := json.Marshal(discoveredService{Name: "api"})
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
	if _, ok := payload["service"]; ok {
		t.Fatalf("legacy service field must not be emitted: %v", payload["service"])
	}
}
