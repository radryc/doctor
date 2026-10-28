package query

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFilterPrometheusDataByIdentity(t *testing.T) {
	data := map[string]any{"result": []any{
		map[string]any{"metric": map[string]string{
			"guardian_partition": "monofs",
			"guardian_intent":    "monofs-storage",
			"guardian_asset":     "node-a",
		}},
		map[string]any{"metric": map[string]string{
			"guardian_partition": "monofs",
			"guardian_intent":    "monofs-storage",
			"guardian_asset":     "node-b",
		}},
		map[string]any{"metric": map[string]string{
			"guardian_partition": "doctor",
			"guardian_intent":    "query",
			"guardian_asset":     "query",
		}},
	}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?partition=monofs&intent=monofs-storage&asset=node-a", nil)
	filtered := filterPrometheusDataByIdentity(req, data)
	result, ok := filtered["result"].([]any)
	if !ok {
		t.Fatalf("result has type %T", filtered["result"])
	}
	if len(result) != 1 {
		t.Fatalf("filtered result = %+v, want one series", result)
	}
	metric := result[0].(map[string]any)["metric"].(map[string]string)
	if metric["guardian_asset"] != "node-a" {
		t.Fatalf("metric = %+v, want node-a", metric)
	}
}

func TestFilterPrometheusDataByIdentityAWSIntentLabels(t *testing.T) {
	data := map[string]any{"result": []any{
		map[string]any{"metric": map[string]string{
			"source":                      "aws",
			"@aws.tag.guardian.io/intent": "storage",
			"@aws.tag.guardian.io/asset":  "node-a",
		}},
		map[string]any{"metric": map[string]string{
			"source":                      "aws",
			"@aws.tag.guardian.io/intent": "storage",
			"@aws.tag.guardian.io/asset":  "node-b",
		}},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?intent=storage&asset=node-a", nil)
	filtered := filterPrometheusDataByIdentity(req, data)
	result := filtered["result"].([]any)
	if len(result) != 1 {
		t.Fatalf("filtered result = %+v, want AWS identity series", result)
	}
}
