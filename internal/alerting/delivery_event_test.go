package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/delivery"
)

func testEvent() Event {
	return Event{
		SchemaVersion: "doctor.incident/v1",
		Kind:          EventKindIncident,
		Event:         "mitigated",
		Sequence:      4,
		ID:            "inc-1",
		Title:         "CheckoutDown",
		Summary:       "bleeding stopped",
		Status:        "mitigated",
		Severity:      "critical",
		URL:           "http://doctor/#/incidents/inc-1",
		OccurredAt:    time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
		Fields: []EventField{
			{Label: "State", Value: "mitigated"},
			{Label: "Owner", Value: "ada"},
		},
	}
}

func TestReceiverSupportsIncidents(t *testing.T) {
	if !ReceiverSupports(Receiver{Type: "teams"}, EventKindIncident) {
		t.Fatal("teams should accept incidents by default")
	}
	if ReceiverSupports(Receiver{Type: "pagerduty"}, EventKindIncident) {
		t.Fatal("pagerduty should not accept incidents")
	}
	if ReceiverSupports(Receiver{Type: "webhook", Events: []string{EventKindAlert}}, EventKindIncident) {
		t.Fatal("explicit alert-only receiver should reject incidents")
	}
}

func TestFCBIncidentUsesRuntimeBackendAndTarget(t *testing.T) {
	var topic string
	var payload delivery.AndroidNotification
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topic = r.Header.Get("X-FCM-Topic")
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	dispatcher := &Dispatcher{FCB: delivery.FCBConfig{
		Enabled:    true,
		BackendURL: server.URL,
	}}
	receiver := Receiver{Name: "mobile", Type: "fcb", Target: "doctor-partition-payments"}
	if err := dispatcher.sendEvent(context.Background(), receiver, testEvent()); err != nil {
		t.Fatal(err)
	}
	if topic != "doctor-partition-payments" {
		t.Fatalf("X-FCM-Topic = %q", topic)
	}
	if payload.SchemaVersion != "doctor.android/v1" || payload.ID != "inc-1" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestTeamsIncidentCardPayload(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	dispatcher := &Dispatcher{}
	err := dispatcher.sendEvent(context.Background(), Receiver{Name: "teams", Type: "teams", URL: server.URL}, testEvent())
	if err != nil {
		t.Fatal(err)
	}
	if captured["@type"] != "MessageCard" {
		t.Fatalf("card type = %v", captured["@type"])
	}
	sections, _ := captured["sections"].([]any)
	if len(sections) == 0 {
		t.Fatal("expected sections")
	}
}

func TestTeamsAlertPayload(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	dispatcher := &Dispatcher{}
	alert := Alert{
		Fingerprint: "fp-1",
		Source:      "test",
		Tenant:      "default",
		Partition:   "doctor",
		Status:      StatusFiring,
		Severity:    "critical",
		Labels:      map[string]string{"alertname": "CheckoutDown"},
		Annotations: map[string]string{"summary": "checkout is down"},
		StartsAt:    time.Now(),
	}
	if err := dispatcher.send(context.Background(), Receiver{Name: "teams", Type: "teams", URL: server.URL}, alert); err != nil {
		t.Fatal(err)
	}
	if captured["@type"] != "MessageCard" {
		t.Fatalf("card type = %v", captured["@type"])
	}
}

func TestZoomIncidentPayload(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	dispatcher := &Dispatcher{}
	if err := dispatcher.sendEvent(context.Background(), Receiver{Name: "zoom", Type: "zoom", URL: server.URL}, testEvent()); err != nil {
		t.Fatal(err)
	}
	content, ok := captured["content"].(map[string]any)
	if !ok || content["head"] == nil {
		t.Fatalf("unexpected zoom payload: %#v", captured)
	}
}

func TestWebhookIncidentPayloadHasSchema(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Doctor-Event-Kind") != EventKindIncident {
			t.Errorf("missing event kind header")
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	dispatcher := &Dispatcher{}
	if err := dispatcher.sendEvent(context.Background(), Receiver{Name: "hook", Type: "webhook", URL: server.URL}, testEvent()); err != nil {
		t.Fatal(err)
	}
	if captured["schemaVersion"] != "doctor.incident/v1" {
		t.Fatalf("schema = %v", captured["schemaVersion"])
	}
	if captured["kind"] != EventKindIncident {
		t.Fatalf("kind = %v", captured["kind"])
	}
}
