package translate

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TraceTag is one trace tag matcher. Regex is set when the query used =~ or !~;
// Value then holds the raw pattern.
type TraceTag struct {
	Key   string
	Value string
	Regex *regexp.Regexp
}

// TraceQuery is a translated TraceQL subset request.
//
// The subset covers the constructs emitted by Grafana's Traces Drilldown and
// Explore trace search:
//
//	{}                                              match all
//	{ nestedSetParent<0 && true }                   root spans
//	{ resource.service.name != nil }                attribute presence
//	{ service.name = "api" && status = "error" }    exact matchers
//	{ kind = server }                               bare enum values
//	{ span.db.system.name != "" }                   empty-string exclusion
//	{ name =~ "GET .*" }                            regex matchers
//	{ duration >= 1s && duration <= 2s }            span duration bounds
//	{ a = "x" || a = "y" }                          same-key disjunction
type TraceQuery struct {
	Service string
	// Tags are positive matchers (= and =~). Regex is set for =~.
	Tags []TraceTag
	// Exists lists keys required to be present (key != nil).
	Exists []string
	// Absent lists keys required to be missing (key = nil).
	Absent []string
	// NotTags are negative matchers (!= and !~). Regex is set for !~.
	NotTags []TraceTag
	// RootOnly selects spans without a parent (nestedSetParent<0).
	RootOnly bool
	// MatchAll is set for the empty expression {}.
	MatchAll bool
	// MatchNone is set when the query contains a literal `false` clause.
	MatchNone bool
	// MinSpanDuration/MaxSpanDuration bound the span duration (duration
	// comparisons). Zero means unbounded.
	MinSpanDuration time.Duration
	MaxSpanDuration time.Duration
}

// Empty reports whether no matchers were parsed.
func (q TraceQuery) Empty() bool {
	return q.Service == "" &&
		len(q.Tags) == 0 &&
		len(q.Exists) == 0 &&
		len(q.Absent) == 0 &&
		len(q.NotTags) == 0 &&
		!q.RootOnly &&
		!q.MatchAll &&
		!q.MatchNone &&
		q.MinSpanDuration == 0 &&
		q.MaxSpanDuration == 0
}

// traceOps are the comparison operators recognised in span filters, longest
// first so `=~` wins over `=`.
var traceOps = []string{"!~", "=~", "!=", "<=", ">=", "<", ">", "="}

// ParseTraceQL parses the supported TraceQL subset. Supported keys include the
// span intrinsics (name, status, kind, duration, nestedSetParent), service
// name aliases (.service.name, resource.service.name), scoped attributes
// (resource.<attr>, span.<attr>), and bare attribute names.
func ParseTraceQL(query string) (TraceQuery, error) {
	query = strings.TrimSpace(query)
	if !strings.HasPrefix(query, "{") || !strings.HasSuffix(query, "}") {
		return TraceQuery{}, fmt.Errorf("%w: traceql must be a brace expression: %q", ErrUnsupported, query)
	}
	inner := strings.TrimSpace(query[1 : len(query)-1])
	if containsUnquotedBrace(inner) {
		return TraceQuery{}, fmt.Errorf("%w: nested braces or structural operators are not supported: %q", ErrUnsupported, query)
	}
	if inner == "" {
		return TraceQuery{MatchAll: true}, nil
	}

	alternatives := splitTraceQLClauses(inner, "||")
	if len(alternatives) > 1 {
		merged, ok := mergeSameKeyAlternatives(alternatives)
		if !ok {
			return TraceQuery{}, fmt.Errorf("%w: || is only supported between matchers on the same key: %q", ErrUnsupported, query)
		}
		alternatives = []string{merged}
	}

	result := TraceQuery{}
	for _, clause := range splitTraceQLClauses(alternatives[0], "&&") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		if err := result.applyClause(clause); err != nil {
			return TraceQuery{}, err
		}
	}
	if result.Empty() {
		return TraceQuery{}, fmt.Errorf("%w: no supported matchers in %q", ErrUnsupported, query)
	}
	return result, nil
}

func (q *TraceQuery) applyClause(clause string) error {
	switch strings.ToLower(clause) {
	case "true":
		return nil
	case "false":
		q.MatchNone = true
		return nil
	}

	key, op, raw, ok := parseTraceQLComparison(clause)
	if !ok {
		return fmt.Errorf("%w: unsupported traceql matcher %q", ErrUnsupported, clause)
	}
	if key == "" {
		return fmt.Errorf("%w: empty attribute name in %q", ErrUnsupported, clause)
	}

	normalized := NormalizeTraceKey(key)
	switch normalized {
	case "nestedSetParent":
		if (op == "<" || op == "<=") && strings.TrimSpace(raw) == "0" {
			q.RootOnly = true
			return nil
		}
		if op == "=" && strings.TrimSpace(raw) == "-1" {
			q.RootOnly = true
			return nil
		}
		return fmt.Errorf("%w: unsupported nestedSetParent comparison %q", ErrUnsupported, clause)
	case "duration":
		return q.applyDuration(op, raw, clause)
	}

	switch op {
	case "=", "=~", "!=", "!~":
	default:
		return fmt.Errorf("%w: unsupported operator %q in %q", ErrUnsupported, op, clause)
	}

	value, isNil, err := parseTraceQLValue(raw)
	if err != nil {
		return err
	}
	if isNil {
		switch op {
		case "!=":
			q.Exists = append(q.Exists, normalized)
		case "=":
			q.Absent = append(q.Absent, normalized)
		default:
			return fmt.Errorf("%w: nil is only valid with = or != in %q", ErrUnsupported, clause)
		}
		return nil
	}

	switch op {
	case "=~", "!~":
		pattern := value
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("%w: invalid regex %q in %q", ErrUnsupported, pattern, clause)
		}
		tag := TraceTag{Key: normalized, Value: pattern, Regex: re}
		if op == "=~" {
			q.Tags = append(q.Tags, tag)
		} else {
			q.NotTags = append(q.NotTags, tag)
		}
		return nil
	case "!=":
		q.NotTags = append(q.NotTags, TraceTag{Key: normalized, Value: normalizeTraceValue(normalized, value)})
		return nil
	}

	if normalized == "service.name" {
		if q.Service != "" {
			return fmt.Errorf("%w: multiple service filters", ErrUnsupported)
		}
		q.Service = value
		return nil
	}
	q.Tags = append(q.Tags, TraceTag{Key: normalized, Value: normalizeTraceValue(normalized, value)})
	return nil
}

func (q *TraceQuery) applyDuration(op, raw, clause string) error {
	duration, err := parseTraceQLDuration(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: invalid duration in %q: %v", ErrUnsupported, clause, err)
	}
	switch op {
	case ">", ">=":
		q.MinSpanDuration = duration
	case "<", "<=":
		q.MaxSpanDuration = duration
	default:
		return fmt.Errorf("%w: unsupported duration comparison %q", ErrUnsupported, clause)
	}
	return nil
}

// NormalizeTraceKey canonicalises attribute aliases to the names stored in
// Doctor's trace records.
func NormalizeTraceKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.TrimPrefix(key, ".")
	switch key {
	case "service", "service.name", "resource.service.name":
		return "service.name"
	case "span.name":
		return "name"
	case "span.status", "status.code":
		return "status"
	case "span.kind":
		return "kind"
	case "resource.service":
		return "service.name"
	}
	return key
}

// normalizeTraceValue maps TraceQL enum spellings onto Doctor's stored OTLP
// enum values.
func normalizeTraceValue(key, value string) string {
	switch key {
	case "status":
		switch strings.ToLower(value) {
		case "error":
			return "STATUS_CODE_ERROR"
		case "ok":
			return "STATUS_CODE_OK"
		case "unset":
			return "STATUS_CODE_UNSET"
		}
	case "kind":
		switch strings.ToLower(value) {
		case "server":
			return "SPAN_KIND_SERVER"
		case "client":
			return "SPAN_KIND_CLIENT"
		case "producer":
			return "SPAN_KIND_PRODUCER"
		case "consumer":
			return "SPAN_KIND_CONSUMER"
		case "internal":
			return "SPAN_KIND_INTERNAL"
		case "unspecified", "unset":
			return "SPAN_KIND_UNSPECIFIED"
		}
	}
	return value
}

// containsUnquotedBrace reports whether the expression contains a brace
// outside a quoted string (structural operators, nested spansets).
func containsUnquotedBrace(inner string) bool {
	var (
		inQuotes bool
		escaped  bool
	)
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inQuotes:
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case !inQuotes && (c == '{' || c == '}'):
			return true
		}
	}
	return false
}

// parseTraceQLComparison splits a clause into key, operator, and raw value,
// ignoring operator characters inside quoted strings.
func parseTraceQLComparison(clause string) (key, op, value string, ok bool) {
	var (
		inQuotes bool
		escaped  bool
	)
	for i := 0; i < len(clause); i++ {
		c := clause[i]
		switch {
		case escaped:
			escaped = false
			continue
		case c == '\\' && inQuotes:
			escaped = true
			continue
		case c == '"':
			inQuotes = !inQuotes
			continue
		case inQuotes:
			continue
		}
		for _, candidate := range traceOps {
			if strings.HasPrefix(clause[i:], candidate) {
				return strings.TrimSpace(clause[:i]), candidate, strings.TrimSpace(clause[i+len(candidate):]), true
			}
		}
	}
	return "", "", "", false
}

// parseTraceQLValue parses a matcher value: nil, a quoted string with escapes,
// or a bare literal (numbers, booleans, enum names).
func parseTraceQLValue(raw string) (value string, isNil bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "nil" {
		return "", true, nil
	}
	if strings.HasPrefix(raw, `"`) {
		if len(raw) < 2 || !strings.HasSuffix(raw, `"`) {
			return "", false, fmt.Errorf("%w: unterminated string literal %q", ErrUnsupported, raw)
		}
		return unescapeTraceQLString(raw[1 : len(raw)-1]), false, nil
	}
	if raw == "" || strings.ContainsAny(raw, ` "'`) {
		return "", false, fmt.Errorf("%w: invalid matcher value %q", ErrUnsupported, raw)
	}
	return raw, false, nil
}

func unescapeTraceQLString(inner string) string {
	var builder strings.Builder
	escaped := false
	for _, r := range inner {
		switch {
		case escaped:
			builder.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func parseTraceQLDuration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty duration")
	}
	return time.ParseDuration(raw)
}

// splitTraceQLClauses splits on a top-level separator (&& or ||), honouring
// quoted strings and backslash escapes.
func splitTraceQLClauses(inner, sep string) []string {
	parts := make([]string, 0)
	var (
		builder  strings.Builder
		inQuotes bool
		escaped  bool
	)
	flush := func() {
		parts = append(parts, builder.String())
		builder.Reset()
	}
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case escaped:
			builder.WriteByte(c)
			escaped = false
		case c == '\\' && inQuotes:
			builder.WriteByte(c)
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
			builder.WriteByte(c)
		case !inQuotes && strings.HasPrefix(inner[i:], sep):
			flush()
			i += len(sep) - 1
		default:
			builder.WriteByte(c)
		}
	}
	flush()
	return parts
}

// mergeSameKeyAlternatives folds `key = "a" || key = "b"` (and regex forms)
// into a single regex matcher on that key. Anything else is unsupported.
func mergeSameKeyAlternatives(alternatives []string) (string, bool) {
	type altMatcher struct {
		key   string
		op    string
		value string
	}
	parsed := make([]altMatcher, 0, len(alternatives))
	for _, alternative := range alternatives {
		alternative = strings.TrimSpace(alternative)
		if alternative == "" {
			continue
		}
		if len(splitTraceQLClauses(alternative, "&&")) > 1 {
			return "", false
		}
		key, op, raw, ok := parseTraceQLComparison(alternative)
		if !ok || (op != "=" && op != "=~") {
			return "", false
		}
		value, isNil, err := parseTraceQLValue(raw)
		if err != nil || isNil {
			return "", false
		}
		parsed = append(parsed, altMatcher{key: NormalizeTraceKey(key), op: op, value: value})
	}
	if len(parsed) == 0 {
		return "", false
	}

	key := parsed[0].key
	anyRegex := false
	for _, matcher := range parsed {
		if matcher.key != key {
			return "", false
		}
		if matcher.op == "=~" {
			anyRegex = true
		}
	}

	parts := make([]string, 0, len(parsed))
	for _, matcher := range parsed {
		switch {
		case matcher.op == "=~":
			parts = append(parts, "(?:"+matcher.value+")")
		case anyRegex:
			parts = append(parts, regexp.QuoteMeta(matcher.value))
		default:
			parts = append(parts, regexp.QuoteMeta(matcher.value))
		}
	}
	pattern := strings.Join(parts, "|")
	if !anyRegex {
		pattern = "^(?:" + pattern + ")$"
	}
	return fmt.Sprintf(`%s =~ %q`, key, pattern), true
}
