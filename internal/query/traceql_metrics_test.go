package query

import "testing"

// The Service RED preset's "Top Failing Operations" panel must use valid
// TraceQL metrics syntax: filters live inside the brace expression and the
// span-name grouping follows the rate() call.
func TestParseTraceQLMetricsServiceREDTopFailing(t *testing.T) {
	parsed, err := parseTraceQLMetricsQuery(
		`{resource.service.name=~"guardian" && status = error} | rate() by (span.name)`,
	)
	if err != nil {
		t.Fatalf("parseTraceQLMetricsQuery: %v", err)
	}
	if parsed.Function != "rate" {
		t.Fatalf("function = %q, want rate", parsed.Function)
	}
	if len(parsed.GroupBy) != 1 {
		t.Fatalf("group-by = %#v, want one span.name entry", parsed.GroupBy)
	}
	if parsed.GroupBy[0].Label != "span.name" || parsed.GroupBy[0].Key != "name" {
		t.Fatalf("group-by = %#v, want span.name -> name", parsed.GroupBy[0])
	}

	foundStatus := false
	for _, tag := range parsed.Search.Tags {
		if tag.Key == "status" && tag.Value == "STATUS_CODE_ERROR" {
			foundStatus = true
		}
	}
	if !foundStatus {
		t.Fatalf("status=error filter not translated: %#v", parsed.Search.Tags)
	}
}
