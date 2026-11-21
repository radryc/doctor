package delivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFCBSendUsesTargetAndRuntimeBackend(t *testing.T) {
	var (
		body   []byte
		topic  string
		target string
		sig    string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		topic = r.Header.Get("X-FCM-Topic")
		target = r.Header.Get("X-FCB-Target")
		sig = r.Header.Get("X-Doctor-Signature")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	runtime := Runtime{FCB: FCBConfig{
		Enabled:       true,
		BackendURL:    server.URL,
		WebhookSecret: "shared-secret",
		Timeout:       2 * time.Second,
	}}
	receiver := Receiver{Name: "mobile-payments", Type: "fcb", Target: "doctor-partition-payments"}
	err := DefaultRegistry().Send(context.Background(), runtime, receiver, Message{
		Kind:       KindIncident,
		Event:      "opened",
		ID:         "inc-1",
		Title:      "CheckoutDown",
		Summary:    "1 alert(s) fired",
		Status:     "open",
		Severity:   "critical",
		OccurredAt: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
		Fields:     []EventField{{Label: "Partition", Value: "payments"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if topic != "doctor-partition-payments" {
		t.Fatalf("X-FCM-Topic = %q", topic)
	}
	if target != "doctor-partition-payments" {
		t.Fatalf("X-FCB-Target = %q", target)
	}

	mac := hmac.New(sha256.New, []byte("shared-secret"))
	_, _ = mac.Write(body)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig != wantSig {
		t.Fatalf("signature = %q, want %q", sig, wantSig)
	}

	var payload AndroidNotification
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SchemaVersion != "doctor.android/v1" || payload.Title != "CheckoutDown" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	if payload.Data["Partition"] != "payments" {
		t.Fatalf("data = %#v", payload.Data)
	}
}

func TestFCBReceiverURLOverridesRuntimeBackend(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.Header.Get("X-FCM-Topic"); got != "doctor-partition-payments" {
			t.Errorf("X-FCM-Topic = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// The global backend is disabled, but an explicit receiver URL still works.
	runtime := Runtime{FCB: FCBConfig{Enabled: false, BackendURL: "http://127.0.0.1:1"}}
	receiver := Receiver{Name: "mobile", Type: "fcb", URL: server.URL, Target: "doctor-partition-payments"}
	if err := DefaultRegistry().Send(context.Background(), runtime, receiver, Message{Kind: KindAlert, ID: "a-1"}); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
}

func TestFCBSendRequiresConfiguredBackend(t *testing.T) {
	receiver := Receiver{Name: "mobile", Type: "fcb", Target: "doctor-partition-payments"}
	err := DefaultRegistry().Send(context.Background(), Runtime{}, receiver, Message{Kind: KindAlert, ID: "a-1"})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("error = %v, want not configured", err)
	}
}

func TestFCBValidate(t *testing.T) {
	// An empty target is valid: the backend decides (on-call rotation or its
	// own default topic).
	if err := (FCBSender{}).Validate(Receiver{Name: "mobile", Type: "fcb"}); err != nil {
		t.Fatalf("Validate() rejected a receiver without a target: %v", err)
	}
	if err := (FCBSender{}).Validate(Receiver{Name: "mobile", Type: "fcb", Target: "bad topic"}); err == nil {
		t.Fatal("Validate() accepted a topic with a space")
	}
	if err := (FCBSender{}).Validate(Receiver{Name: "mobile", Type: "fcb", Target: "doctor-partition-payments"}); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := (FCBSender{}).Validate(Receiver{Name: "mobile", Type: "fcb", Headers: map[string]string{"X-FCM-Topic": "doctor-partition-payments"}}); err != nil {
		t.Fatalf("Validate() header fallback error = %v", err)
	}
	if err := (FCBSender{}).Validate(Receiver{Name: "mobile", Type: "fcb", Headers: map[string]string{"X-FCM-Token": "device-token"}}); err != nil {
		t.Fatalf("Validate() token fallback error = %v", err)
	}
}

func TestFCBHeaderTopicFallback(t *testing.T) {
	var topic string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topic = r.Header.Get("X-FCM-Topic")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	receiver := Receiver{
		Name:    "mobile",
		Type:    "fcb",
		URL:     server.URL,
		Headers: map[string]string{"X-FCM-Topic": "doctor-partition-platform"},
	}
	if err := DefaultRegistry().Send(context.Background(), Runtime{}, receiver, Message{Kind: KindAlert, ID: "a-1"}); err != nil {
		t.Fatal(err)
	}
	if topic != "doctor-partition-platform" {
		t.Fatalf("X-FCM-Topic = %q", topic)
	}
}

func TestFCBSendWithoutTargetLetsBackendChoose(t *testing.T) {
	var topic, target string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topic = r.Header.Get("X-FCM-Topic")
		target = r.Header.Get("X-FCB-Target")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	runtime := Runtime{FCB: FCBConfig{Enabled: true, BackendURL: server.URL}}
	receiver := Receiver{Name: "mobile", Type: "fcb"}
	if err := DefaultRegistry().Send(context.Background(), runtime, receiver, Message{Kind: KindAlert, ID: "a-1"}); err != nil {
		t.Fatal(err)
	}
	if topic != "" || target != "" {
		t.Fatalf("expected no target headers, got topic=%q target=%q", topic, target)
	}
}

func TestFCBSendUsesConfiguredDefaultTopic(t *testing.T) {
	var topic, target string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topic = r.Header.Get("X-FCM-Topic")
		target = r.Header.Get("X-FCB-Target")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	runtime := Runtime{FCB: FCBConfig{
		Enabled:      true,
		BackendURL:   server.URL,
		DefaultTopic: "doctor-alerts",
	}}
	receiver := Receiver{Name: "mobile", Type: "fcb"}
	if err := DefaultRegistry().Send(context.Background(), runtime, receiver, Message{Kind: KindAlert, ID: "a-1"}); err != nil {
		t.Fatal(err)
	}
	if topic != "doctor-alerts" || target != "doctor-alerts" {
		t.Fatalf("topic=%q target=%q, want doctor-alerts", topic, target)
	}
}

func TestFCBIsIncidentCapable(t *testing.T) {
	if !IncidentCapable("fcb") {
		t.Fatal("fcb receiver should be registered as incident-capable")
	}
}
