package delivery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeSender struct {
	got Message
}

func (f *fakeSender) Type() string            { return "fake" }
func (f *fakeSender) Kinds() []string         { return []string{KindAlert, KindIncident} }
func (f *fakeSender) Validate(Receiver) error { return nil }
func (f *fakeSender) Send(_ context.Context, _ Runtime, _ Receiver, message Message) error {
	f.got = message
	return nil
}

func TestRegistryIsExtensible(t *testing.T) {
	registry := NewRegistry()
	sender := &fakeSender{}
	registry.Register(sender)
	receiver := Receiver{Name: "custom", Type: "fake"}
	if err := registry.Send(context.Background(), Runtime{}, receiver, Message{Kind: KindIncident, ID: "inc-1", Title: "Checkout"}); err != nil {
		t.Fatal(err)
	}
	if sender.got.ID != "inc-1" {
		t.Fatalf("sender did not receive the message: %#v", sender.got)
	}
	if !IncidentCapable("android") {
		t.Fatal("android receiver should be registered as incident-capable")
	}
}

func TestWebhookAlertKeepsRawPayload(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	raw := []byte(`{"fingerprint":"fp-1","status":"firing"}`)
	err := DefaultRegistry().Send(context.Background(), Runtime{}, Receiver{Name: "hook", Type: "webhook", URL: server.URL}, Message{
		Kind: KindAlert,
		ID:   "fp-1",
		Raw:  raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(captured) != string(raw) {
		t.Fatalf("alert payload changed: %s", captured)
	}
}

func TestWebhookIncidentHeaders(t *testing.T) {
	var kind, event, idempotency string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind = r.Header.Get("X-Doctor-Event-Kind")
		event = r.Header.Get("X-Doctor-Event")
		idempotency = r.Header.Get("X-Doctor-Idempotency-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	err := DefaultRegistry().Send(context.Background(), Runtime{}, Receiver{Name: "hook", Type: "webhook", URL: server.URL}, Message{
		Kind:     KindIncident,
		Event:    "opened",
		ID:       "inc-1",
		Sequence: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindIncident || event != "opened" {
		t.Fatalf("headers = %q %q", kind, event)
	}
	if idempotency != "inc-1/2" {
		t.Fatalf("idempotency = %q", idempotency)
	}
}

func TestAndroidPayload(t *testing.T) {
	var captured AndroidNotification
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	err := DefaultRegistry().Send(context.Background(), Runtime{}, Receiver{Name: "android", Type: "android", URL: server.URL}, Message{
		Kind:       KindIncident,
		Event:      "mitigated",
		ID:         "inc-1",
		Title:      "CheckoutDown",
		Summary:    "bleeding stopped",
		Status:     "mitigated",
		Severity:   "critical",
		URL:        "https://doctor/#/incidents/inc-1",
		OccurredAt: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
		Fields:     []EventField{{Label: "State", Value: "mitigated"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.SchemaVersion != "doctor.android/v1" || captured.Title != "CheckoutDown" {
		t.Fatalf("unexpected payload: %#v", captured)
	}
	if captured.Priority != "high" {
		t.Fatalf("priority = %q", captured.Priority)
	}
	if captured.Data["State"] != "mitigated" {
		t.Fatalf("data = %#v", captured.Data)
	}
}
