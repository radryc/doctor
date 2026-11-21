package incident

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newAgentStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/investigations" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Nightwatch-Partition-Access") != "doctor" {
			t.Errorf("missing partition scope header")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if payload["externalRef"] != "doctor:inc-1" {
			t.Errorf("externalRef = %v", payload["externalRef"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"summary":               "Checkout error rate spiked after a deploy.",
			"root_cause_partition":  "doctor",
			"root_cause_service":    "checkout",
			"confidence":            0.81,
			"recommended_action":    "Roll back the checkout deployment",
			"recommendation_target": "doctor/checkout",
			"rationale":             "5xx errors began at the deploy timestamp.",
			"model":                 "test-model",
			"improvements": []map[string]any{{
				"title":      "Add a deploy marker annotation",
				"category":   "observability",
				"rationale":  "Correlate deploys with error rate.",
				"target":     "doctor/checkout",
				"confidence": 0.7,
			}},
			"evidence": []map[string]any{{
				"kind":           "metrics",
				"query":          "rate(http_requests_total[5m])",
				"result_summary": "5xx doubled",
				"source":         "doctor",
			}},
		})
	}))
}
