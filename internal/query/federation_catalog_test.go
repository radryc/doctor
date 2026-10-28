package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/doctor/internal/federation"
)

func newFederationCatalog(t *testing.T) *federation.Manager {
	t.Helper()
	catalog, err := federation.NewCatalog([]federation.Target{
		{
			ID:           "aws-111-us-east-1",
			Account:      "111111111111",
			Region:       "us-east-1",
			Partitions:   []string{"div-a-orders"},
			Endpoint:     "http://pusher-a",
			Token:        "token-a",
			Provider:     "aws",
			Capabilities: []federation.Signal{federation.SignalMetrics, federation.SignalLogs, federation.SignalTraces},
			MetricNames:  []string{"orders_requests_total", "orders_errors_total"},
			LogGroups:    []string{"/aws/lambda/orders"},
			XrayServices: []string{"orders-api"},
		},
		{
			ID:           "aws-222-us-east-1",
			Account:      "222222222222",
			Region:       "us-east-1",
			Partitions:   []string{"div-a-billing"},
			Endpoint:     "http://pusher-b",
			Token:        "token-b",
			Provider:     "aws",
			Capabilities: []federation.Signal{federation.SignalMetrics, federation.SignalLogs},
			MetricNames:  []string{"billing_requests_total"},
			LogGroups:    []string{"/aws/lambda/billing"},
		},
		{
			ID:           "aws-333-us-west-2",
			Account:      "333333333333",
			Region:       "us-west-2",
			Partitions:   []string{"div-b-inventory"},
			Endpoint:     "http://pusher-c",
			Token:        "token-c",
			Provider:     "aws",
			Capabilities: []federation.Signal{federation.SignalTraces},
			XrayServices: []string{"inventory-api"},
		},
	}, "catalog-test")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return federation.NewManager(catalog, federation.Options{})
}

func federationTestService(t *testing.T) *Service {
	t.Helper()
	svc := &Service{defaultTenant: "default"}
	svc.SetPolicyGate(newCatalogGate(t))
	svc.SetAWSBackend(newFederationCatalog(t))
	return svc
}

func federationRequest(t *testing.T, svc *Service, handler http.HandlerFunc, email, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), catalogIdentity(email)))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestFederationStatusFiltersTargetsAndPartitions(t *testing.T) {
	svc := federationTestService(t)

	type statusResponse struct {
		Enabled        bool           `json:"enabled"`
		Targets        int            `json:"targets"`
		AccountCount   int            `json:"account_count"`
		Accounts       map[string]int `json:"accounts"`
		Partitions     map[string]int `json:"partitions"`
		PartitionCount int            `json:"partition_count"`
		Regions        map[string]int `json:"regions"`
	}

	status := func(email string) statusResponse {
		rec := federationRequest(t, svc, svc.handleFederationStatus, email, "/v1/federation/status")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body statusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	usera := status("usera@example.com")
	if !usera.Enabled || usera.Targets != 2 || usera.AccountCount != 2 {
		t.Fatalf("usera status = %+v, want 2 targets and 2 accounts", usera)
	}
	if _, ok := usera.Partitions["div-b-inventory"]; ok {
		t.Fatalf("usera saw div-b partition: %+v", usera.Partitions)
	}
	if usera.PartitionCount != 2 {
		t.Fatalf("usera partition_count = %d, want 2", usera.PartitionCount)
	}

	userb := status("userb@example.com")
	if userb.Targets != 1 || userb.PartitionCount != 1 {
		t.Fatalf("userb status = %+v, want 1 target and 1 partition", userb)
	}
	if _, ok := userb.Partitions["div-b-inventory"]; !ok {
		t.Fatalf("userb missing div-b partition: %+v", userb.Partitions)
	}

	admin := status("admin@example.com")
	if admin.Targets != 3 || admin.AccountCount != 3 || admin.PartitionCount != 3 {
		t.Fatalf("admin status = %+v, want 3 targets/accounts/partitions", admin)
	}
}

func TestFederationTargetsFiltersPartitionsAndIncludesMetadata(t *testing.T) {
	svc := federationTestService(t)

	type targetsResponse struct {
		Targets []struct {
			ID            string   `json:"id"`
			Partitions    []string `json:"partitions"`
			MetricCount   int      `json:"metric_count"`
			LogGroupCount int      `json:"log_group_count"`
			MetricNames   []string `json:"metric_names"`
			LogGroups     []string `json:"log_groups"`
			XrayServices  []string `json:"xray_services"`
		} `json:"targets"`
		Total   int `json:"total"`
		Matched int `json:"matched"`
	}

	list := func(email, query string) targetsResponse {
		rec := federationRequest(t, svc, svc.handleFederationTargets, email, "/v1/federation/targets"+query)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body targetsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	usera := list("usera@example.com", "?limit=10")
	if usera.Total != 2 || usera.Matched != 2 {
		t.Fatalf("usera page = %+v, want 2 visible targets", usera)
	}
	for _, target := range usera.Targets {
		for _, partition := range target.Partitions {
			if partition == "div-b-inventory" {
				t.Fatalf("usera saw div-b target: %+v", target)
			}
		}
	}

	userb := list("userb@example.com", "?limit=10")
	if userb.Total != 1 || len(userb.Targets) != 1 || userb.Targets[0].ID != "aws-333-us-west-2" {
		t.Fatalf("userb page = %+v, want only aws-333", userb)
	}

	admin := list("admin@example.com", "?limit=10")
	if admin.Total != 3 || len(admin.Targets) != 3 {
		t.Fatalf("admin page = %+v, want all targets", admin)
	}

	detail := list("usera@example.com", "?q=aws-111&include_metadata=1")
	if detail.Matched != 1 || len(detail.Targets) != 1 {
		t.Fatalf("detail page = %+v, want one matched target", detail)
	}
	if detail.Targets[0].MetricCount != 2 || len(detail.Targets[0].MetricNames) != 2 {
		t.Fatalf("detail metadata = %+v, want metric names and counts", detail.Targets[0])
	}
	if len(detail.Targets[0].LogGroups) != 1 || len(detail.Targets[0].XrayServices) != 1 {
		t.Fatalf("detail metadata = %+v, want log groups and X-Ray services", detail.Targets[0])
	}
}

func TestFederationPlanIsAdminOnly(t *testing.T) {
	svc := federationTestService(t)

	userRec := federationRequest(t, svc, svc.handleFederationPlan, "usera@example.com",
		"/v1/federation/plan?signal=logs&query=%7B%7D")
	if userRec.Code != http.StatusForbidden {
		t.Fatalf("non-admin plan status = %d, want 403", userRec.Code)
	}

	adminRec := federationRequest(t, svc, svc.handleFederationPlan, "admin@example.com",
		"/v1/federation/plan?signal=logs&query=%7B%7D")
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin plan status = %d, body = %s", adminRec.Code, adminRec.Body.String())
	}
}
