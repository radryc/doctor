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

func TestParseSelectorRejectsInjectedLabelName(t *testing.T) {
	if _, err := ParseSelector(`{foo OR 1=1 --="x"}`); err == nil {
		t.Fatal("expected an invalid label name error")
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

// TestParseTraceQLGrafanaQueries pins the exact query shapes Grafana's Tempo
// datasource and Traces Drilldown send (see Grafana 12.4.5 search.go and
// traces-drilldown generateMetricsQuery.ts / primary-signals.ts).
func TestParseTraceQLGrafanaQueries(t *testing.T) {
	cases := []struct {
		raw   string
		check func(t *testing.T, q TraceQuery)
	}{
		{
			raw: `{}`,
			check: func(t *testing.T, q TraceQuery) {
				if !q.MatchAll {
					t.Fatalf("want MatchAll, got %#v", q)
				}
			},
		},
		{
			raw: `{nestedSetParent<0 && true}`,
			check: func(t *testing.T, q TraceQuery) {
				if !q.RootOnly {
					t.Fatalf("want RootOnly, got %#v", q)
				}
			},
		},
		{
			raw: `{nestedSetParent<0 && true && resource.service.name != nil}`,
			check: func(t *testing.T, q TraceQuery) {
				if !q.RootOnly || len(q.Exists) != 1 || q.Exists[0] != "service.name" {
					t.Fatalf("got %#v", q)
				}
			},
		},
		{
			raw: `{resource.service.name != nil}`,
			check: func(t *testing.T, q TraceQuery) {
				if len(q.Exists) != 1 || q.Exists[0] != "service.name" {
					t.Fatalf("got %#v", q)
				}
			},
		},
		{
			raw: `{span.db.system.name!=""}`,
			check: func(t *testing.T, q TraceQuery) {
				if len(q.NotTags) != 1 || q.NotTags[0].Key != "span.db.system.name" || q.NotTags[0].Value != "" {
					t.Fatalf("got %#v", q)
				}
			},
		},
		{
			raw: `{kind=server}`,
			check: func(t *testing.T, q TraceQuery) {
				if len(q.Tags) != 1 || q.Tags[0].Value != "SPAN_KIND_SERVER" {
					t.Fatalf("got %#v", q)
				}
			},
		},
		{
			raw: `{status=error}`,
			check: func(t *testing.T, q TraceQuery) {
				if len(q.Tags) != 1 || q.Tags[0].Value != "STATUS_CODE_ERROR" {
					t.Fatalf("got %#v", q)
				}
			},
		},
		{
			raw: `{resource.service.name = "guardian" && duration >= 1s && duration <= 2s}`,
			check: func(t *testing.T, q TraceQuery) {
				if q.Service != "guardian" || q.MinSpanDuration != time.Second || q.MaxSpanDuration != 2*time.Second {
					t.Fatalf("got %#v", q)
				}
			},
		},
		{
			raw: `{name =~ "GET .*"}`,
			check: func(t *testing.T, q TraceQuery) {
				if len(q.Tags) != 1 || q.Tags[0].Regex == nil || !q.Tags[0].Regex.MatchString("GET /health") {
					t.Fatalf("got %#v", q)
				}
			},
		},
		{
			raw: `{name="a && b"}`,
			check: func(t *testing.T, q TraceQuery) {
				if len(q.Tags) != 1 || q.Tags[0].Value != "a && b" {
					t.Fatalf("quoted separator not honoured: %#v", q)
				}
			},
		},
		{
			raw: `{http.method="GET" || http.method="POST"}`,
			check: func(t *testing.T, q TraceQuery) {
				if len(q.Tags) != 1 || q.Tags[0].Regex == nil {
					t.Fatalf("disjunction not merged: %#v", q)
				}
				if !q.Tags[0].Regex.MatchString("GET") || !q.Tags[0].Regex.MatchString("POST") || q.Tags[0].Regex.MatchString("PUT") {
					t.Fatalf("merged regex = %v", q.Tags[0].Regex)
				}
			},
		},
		{
			raw: `{kind=server && status=error && nestedSetParent<0}`,
			check: func(t *testing.T, q TraceQuery) {
				if !q.RootOnly || len(q.Tags) != 2 {
					t.Fatalf("got %#v", q)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			query, err := ParseTraceQL(tc.raw)
			if err != nil {
				t.Fatalf("ParseTraceQL(%q): %v", tc.raw, err)
			}
			tc.check(t, query)
		})
	}
}

func TestParseTraceQLRejectsUnsupported(t *testing.T) {
	for _, raw := range []string{
		`{ .name = "x" } | count() > 1`,
		`not-a-brace`,
		`{http.method="GET" || rpc.method="Ping"}`,
		`{name !=~ "x"}`,
		`{nestedSetParent<1}`,
		`{name =~ "("}`,
	} {
		if _, err := ParseTraceQL(raw); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("ParseTraceQL(%q) err = %v, want ErrUnsupported", raw, err)
		}
	}
}
