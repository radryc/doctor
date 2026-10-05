package translate

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLogQLToESQL(t *testing.T) {
	from := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	query, err := LogQLToESQL(LogQuery{
		Selector: `{service="api", level!="debug", k8s_pod_name=~"api-.*"}`,
		LineFilters: []LogLineFilter{
			{Op: "|=", Value: "boom"},
			{Op: "!~", Value: "ignore.*"},
		},
	}, ESConfig{Index: "logs-*.otel-*", From: from, To: to, Limit: 500})
	if err != nil {
		t.Fatalf("LogQLToESQL: %v", err)
	}
	for _, want := range []string{
		"FROM logs-*.otel-*",
		`resource.attributes.service.name == "api"`,
		`severity_text != "debug"`,
		`attributes.k8s_pod_name RLIKE "^(?s:api-.*)$"`,
		`body.text LIKE "*boom*"`,
		`NOT (body.text RLIKE "ignore.*")`,
		`@timestamp >= "2026-06-01T12:00:00Z"`,
		`@timestamp <= "2026-06-01T13:00:00Z"`,
		"| LIMIT 500",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
}

func TestLogQLToESQLSchemaOverride(t *testing.T) {
	query, err := LogQLToESQL(LogQuery{Selector: `{service="api"}`}, ESConfig{
		Index:  "logs-*",
		Schema: map[string]string{"service": "labels.service", "body": "message"},
	})
	if err != nil {
		t.Fatalf("LogQLToESQL: %v", err)
	}
	if !strings.Contains(query, `labels.service == "api"`) {
		t.Fatalf("schema override missing:\n%s", query)
	}
	if !strings.Contains(query, "FROM logs-*") {
		t.Fatalf("index missing:\n%s", query)
	}
}

func TestLogQLToESQLRejectsUnsupported(t *testing.T) {
	cases := []LogQuery{
		{Selector: `service="api"`},
		{Selector: `{service="api"}`, LineFilters: []LogLineFilter{{Op: "|>", Value: "x"}}},
	}
	for _, tc := range cases {
		if _, err := LogQLToESQL(tc, ESConfig{Index: "logs-*"}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("LogQLToESQL(%#v) err = %v, want ErrUnsupported", tc, err)
		}
	}
	if _, err := LogQLToESQL(LogQuery{Selector: `{service="api"}`}, ESConfig{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing index err = %v, want ErrUnsupported", err)
	}
}

func TestParseSelectorHandlesEscapes(t *testing.T) {
	matchers, err := ParseSelector(`{message="say \"hi\"", path=~"/a\\d+"}`)
	if err != nil {
		t.Fatalf("ParseSelector: %v", err)
	}
	if len(matchers) != 2 {
		t.Fatalf("matchers = %#v", matchers)
	}
	if matchers[0].Value != `say "hi"` {
		t.Fatalf("value = %q", matchers[0].Value)
	}
	if matchers[1].Value != `/a\d+` {
		t.Fatalf("regex = %q", matchers[1].Value)
	}
}

func TestLogQLToBigQuery(t *testing.T) {
	from := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	query, err := LogQLToBigQuery(LogQuery{
		Selector:    `{service="api"}`,
		LineFilters: []LogLineFilter{{Op: "|=", Value: "boom"}, {Op: "|~", Value: "err.*"}},
	}, BQConfig{
		Project: "proj", Dataset: "obs", Table: "logs",
		From: from, To: from.Add(time.Hour), Limit: 100,
	})
	if err != nil {
		t.Fatalf("LogQLToBigQuery: %v", err)
	}
	for _, want := range []string{
		"SELECT * FROM `proj.obs.logs`",
		"service = 'api'",
		"STRPOS(body, 'boom') > 0",
		`REGEXP_CONTAINS(body, r"err.*")`,
		"timestamp >= TIMESTAMP('2026-06-01T12:00:00Z')",
		"ORDER BY timestamp DESC LIMIT 100",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
}

func TestLogQLToBigQueryEscapesQuotes(t *testing.T) {
	query, err := LogQLToBigQuery(LogQuery{Selector: `{message="it's"}`}, BQConfig{Table: "logs"})
	if err != nil {
		t.Fatalf("LogQLToBigQuery: %v", err)
	}
	if !strings.Contains(query, `message = 'it''s'`) {
		t.Fatalf("quote escaping missing:\n%s", query)
	}
}

func TestParseTraceQLSubset(t *testing.T) {
	query, err := ParseTraceQL(`{ .service.name = "api" && .name = "GET /a" && http.method = "GET" }`)
	if err != nil {
		t.Fatalf("ParseTraceQL: %v", err)
	}
	if query.Service != "api" {
		t.Fatalf("service = %q", query.Service)
	}
	if len(query.Tags) != 2 || query.Tags[0].Key != "name" || query.Tags[1].Key != "http.method" {
		t.Fatalf("tags = %#v", query.Tags)
	}
}

func TestParseTraceQLRejectsUnsupported(t *testing.T) {
	for _, raw := range []string{
		`{ .name =~ "x" }`,
		`{ .name = "x" } | count() > 1`,
		`not-a-brace`,
	} {
		if _, err := ParseTraceQL(raw); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("ParseTraceQL(%q) err = %v, want ErrUnsupported", raw, err)
		}
	}
}
