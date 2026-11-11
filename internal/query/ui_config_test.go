package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

func uiConfigFor(t *testing.T, gate bool, email string) map[string]any {
	t.Helper()
	svc := &Service{defaultTenant: "default", defaultPartition: "doctor"}
	if gate {
		svc.SetPolicyGate(newCatalogGate(t))
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/ui/config", nil)
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), catalogIdentity(email)))
	rec := httptest.NewRecorder()
	svc.handleUIConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload
}

func TestUIConfigSelectsAccessibleDefaultPartition(t *testing.T) {
	payload := uiConfigFor(t, true, "usera@example.com")
	if got := payload["default_partition"]; got != "div-a-billing" {
		t.Fatalf("default_partition = %v, want div-a-billing", got)
	}
	var accessible []string
	raw, _ := json.Marshal(payload["accessible_partitions"])
	if err := json.Unmarshal(raw, &accessible); err != nil {
		t.Fatalf("accessible_partitions: %v", err)
	}
	want := []string{"div-a-billing", "div-a-orders"}
	if !reflect.DeepEqual(accessible, want) {
		t.Fatalf("accessible_partitions = %v, want %v", accessible, want)
	}
}

func TestUIConfigKeepsDefaultForAdmin(t *testing.T) {
	payload := uiConfigFor(t, true, "admin@example.com")
	if got := payload["default_partition"]; got != "doctor" {
		t.Fatalf("default_partition = %v, want doctor", got)
	}
	if _, ok := payload["accessible_partitions"]; ok {
		t.Fatalf("admin must not receive a restricted partition list: %v", payload)
	}
}

func TestUIConfigWithoutPolicyKeepsDefault(t *testing.T) {
	payload := uiConfigFor(t, false, "usera@example.com")
	if got := payload["default_partition"]; got != "doctor" {
		t.Fatalf("default_partition = %v, want doctor", got)
	}
	if _, ok := payload["accessible_partitions"]; ok {
		t.Fatalf("unrestricted config must not include a restricted list: %v", payload)
	}
}
