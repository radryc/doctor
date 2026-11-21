package alerting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func signedFCBAckRequest(t *testing.T, body, secret string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/alerts/intake/fcb/ack", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(body))
		req.Header.Set("X-Doctor-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return req
}

func seedFCBAckAlert(t *testing.T, store *Store) Alert {
	t.Helper()
	alert := Alert{
		ID:          "fp-1",
		Fingerprint: "fp-1",
		Source:      "sentinel",
		Tenant:      "default",
		Partition:   "payments",
		Status:      StatusFiring,
		Severity:    "critical",
		Labels:      map[string]string{"alertname": "CheckoutDown", "severity": "critical", "partition": "payments"},
	}
	if err := store.SaveAlert(context.Background(), alert, nil); err != nil {
		t.Fatal(err)
	}
	return alert
}

func TestFCBAckCallbackAcknowledgesAlert(t *testing.T) {
	api, store := testAPI(t)
	api.FCBSecret = []byte("ack-secret")
	seedFCBAckAlert(t, store)

	body := `{"schemaVersion":"doctor.android.ack/v1","pageId":"page-1","alertId":"fp-1","action":"ack","status":"acked","deviceName":"Alice Phone","owner":"alice"}`
	req := signedFCBAckRequest(t, body, "ack-secret")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	alert, err := store.GetAlert(context.Background(), "default", "payments", "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if !alert.Acked || alert.AckedBy != "alice" || alert.AckedAt == nil {
		t.Fatalf("alert not acknowledged: %+v", alert)
	}
}

func TestFCBAckNackClearsAcknowledgement(t *testing.T) {
	api, store := testAPI(t)
	api.FCBSecret = []byte("ack-secret")
	seedFCBAckAlert(t, store)

	ack := signedFCBAckRequest(t, `{"alertId":"fp-1","action":"ack","owner":"alice"}`, "ack-secret")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, ack)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ack status = %d body=%s", rec.Code, rec.Body.String())
	}

	nack := signedFCBAckRequest(t, `{"alertId":"fp-1","action":"nack","owner":"alice","note":"on a flight"}`, "ack-secret")
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, nack)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("nack status = %d body=%s", rec.Code, rec.Body.String())
	}

	alert, err := store.GetAlert(context.Background(), "default", "payments", "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if alert.Acked || alert.AckedBy != "" || alert.AckedAt != nil {
		t.Fatalf("nack did not clear acknowledgement: %+v", alert)
	}
}

func TestFCBAckCallbackUsesPartitionHint(t *testing.T) {
	api, store := testAPI(t)
	api.FCBSecret = []byte("ack-secret")
	seedFCBAckAlert(t, store)

	body := `{"alertId":"fp-1","action":"ack","partition":"payments","deviceId":"dev-2"}`
	req := signedFCBAckRequest(t, body, "ack-secret")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	alert, err := store.GetAlert(context.Background(), "default", "payments", "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if !alert.Acked || alert.AckedBy != "dev-2" {
		t.Fatalf("partition hint not applied: %+v", alert)
	}
}

func TestFCBAckCallbackPropagatesIncidentAck(t *testing.T) {
	api, _ := testAPI(t)
	api.FCBSecret = []byte("ack-secret")
	var got struct {
		tenant string
		id     string
		actor  string
		acked  bool
	}
	api.AckIncident = func(_ context.Context, tenant, id, actor string, acked bool) (bool, error) {
		got.tenant, got.id, got.actor, got.acked = tenant, id, actor, acked
		return true, nil
	}

	body := `{"alertId":"platform-critical-abc123-1","action":"ack","owner":"bob","deviceName":"Bob Phone"}`
	req := signedFCBAckRequest(t, body, "ack-secret")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got.tenant != "default" || got.id != "platform-critical-abc123-1" || got.actor != "bob" || !got.acked {
		t.Fatalf("incident callback = %+v", got)
	}
}

func TestFCBAckCallbackRejectsUnsignedOrInvalidRequests(t *testing.T) {
	api, store := testAPI(t)
	api.FCBSecret = []byte("ack-secret")
	seedFCBAckAlert(t, store)
	handler := api.Handler()

	send := func(req *http.Request) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	bad := signedFCBAckRequest(t, `{"alertId":"fp-1","action":"ack"}`, "wrong-secret")
	if code := send(bad); code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", code)
	}

	unsigned := signedFCBAckRequest(t, `{"alertId":"fp-1","action":"ack"}`, "")
	if code := send(unsigned); code != http.StatusUnauthorized {
		t.Fatalf("unsigned status = %d, want 401", code)
	}

	badAction := signedFCBAckRequest(t, `{"alertId":"fp-1","action":"maybe"}`, "ack-secret")
	if code := send(badAction); code != http.StatusBadRequest {
		t.Fatalf("bad action status = %d, want 400", code)
	}

	missing := signedFCBAckRequest(t, `{"action":"ack"}`, "ack-secret")
	if code := send(missing); code != http.StatusBadRequest {
		t.Fatalf("missing id status = %d, want 400", code)
	}

	valid := signedFCBAckRequest(t, `{"alertId":"fp-1","action":"ack"}`, "ack-secret")
	if code := send(valid); code != http.StatusAccepted {
		t.Fatalf("valid status = %d, want 202", code)
	}
}
