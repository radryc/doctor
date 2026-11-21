package incident

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radryc/doctor/internal/alerting"
)

func TestAPIIncidentLifecycle(t *testing.T) {
	files := alerting.NewMemoryStore()
	alerts := alerting.NewStore(files, "", "/doctor/v1/alerts")
	store := NewStore(files, alerts, "", "/doctor/v1/alerts")
	service := &Service{Store: store, Alerts: alerts, Engines: NewEngineRegistry(), Analyzers: map[string]Analyzer{}, Delivery: false}
	server := httptest.NewServer((&API{Store: store, Service: service, AllowAll: true}).Handler())
	defer server.Close()

	alert := alerting.Alert{
		Fingerprint: "fp-1",
		Source:      "test",
		Tenant:      "default",
		Partition:   "doctor",
		Status:      alerting.StatusFiring,
		Severity:    "critical",
		Labels:      map[string]string{"alertname": "CheckoutDown", "severity": "critical", "partition": "doctor"},
	}
	if err := alerts.SaveAlert(context.Background(), alert, nil); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"name":"platform-critical","tenant":"default","partition":"doctor","severityFloor":"critical","match":[{"name":"severity","operator":"=","value":"critical"}],"groupBy":["partition"],"postmortem":{"engine":"builtin","analysisProvider":"none"}}`)
	response, err := http.Post(server.URL+"/v1/alert-sets?tenant=default&partition=doctor", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create set status = %d", response.StatusCode)
	}
	response.Body.Close()

	if err := service.ReconcileTenant(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	response, err = http.Get(server.URL + "/v1/incidents?tenant=default&partition=doctor")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Incidents []Incident `json:"incidents"`
	}
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(list.Incidents) != 1 {
		t.Fatalf("incidents = %d", len(list.Incidents))
	}
	id := list.Incidents[0].ID

	post := func(path, payload string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader([]byte(payload)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post("/v1/incidents/"+id+"/mitigate?tenant=default&partition=doctor", `{"note":"rolled back","override":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mitigate status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = post("/v1/incidents/"+id+"/resolve?tenant=default&partition=doctor", `{"resolution":"fixed"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	response, err = http.Get(server.URL + "/v1/incidents/" + id + "/postmortem?tenant=default&partition=doctor")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("postmortem status = %d", response.StatusCode)
	}
	var doc Postmortem
	if err := json.NewDecoder(response.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if doc.Status != PostmortemDraft {
		t.Fatalf("postmortem status = %s", doc.Status)
	}
}

func TestAPIRejectsUnsignedAdminWhenAllowAllFalse(t *testing.T) {
	files := alerting.NewMemoryStore()
	alerts := alerting.NewStore(files, "", "/doctor/v1/alerts")
	store := NewStore(files, alerts, "", "/doctor/v1/alerts")
	service := &Service{Store: store, Alerts: alerts, Engines: NewEngineRegistry(), Analyzers: map[string]Analyzer{}, Delivery: false}
	api := &API{
		Store:              store,
		Service:            service,
		AllowAll:           false,
		IdentityHMACSecret: "incident-secret",
		ServiceToken:       "svc-secret",
	}
	handler := api.Handler()

	// Unsigned X-Doctor-Admin: true must be rejected.
	req := httptest.NewRequest(http.MethodGet, "/v1/incidents?tenant=default&partition=doctor", nil)
	req.Header.Set("X-Doctor-User", "attacker")
	req.Header.Set("X-Doctor-Admin", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unsigned admin status = %d, want 404", rec.Code)
	}

	// Signed X-Doctor-Identity-Sig must be accepted.
	req.Header.Set("X-Doctor-Identity-Sig", alerting.SignIdentity("incident-secret", "attacker", "true", ""))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed admin status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	// Valid service token must also be accepted.
	reqSvc := httptest.NewRequest(http.MethodGet, "/v1/incidents?tenant=default&partition=doctor", nil)
	reqSvc.Header.Set("Authorization", "Bearer svc-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, reqSvc)
	if rec.Code != http.StatusOK {
		t.Fatalf("service token status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
}
