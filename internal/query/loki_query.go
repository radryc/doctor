package query

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// This file implements the LogQL subset Doctor needs to serve Grafana correctly.
//
// monofs only understands a stream selector plus line filters, so Doctor parses
// the full query, reduces it to that subset for storage, and applies the
// remaining stages (parsers and label filters) and metric aggregations itself.

type lokiLineFilter struct {
	op    string
	value string
	regex *regexp.Regexp
}

type lokiLabelFilter struct {
	name  string
	op    string
	value string
	regex *regexp.Regexp
}

type lokiMetricPlan struct {
	aggregate string
	groupBy   []string
	rangeDur  time.Duration
}

type lokiPlan struct {
	selector     string
	lineFilters  []lokiLineFilter
	hasLogfmt    bool
	hasJSON      bool
	labelFilters []lokiLabelFilter
	metric       *lokiMetricPlan
}

var lokiParserStages = map[string]struct{}{
	"logfmt":       {},
	"json":         {},
	"line_format":  {},
	"label_format": {},
	"pattern":      {},
	"regexp":       {},
	"unpack":       {},
	"decolorize":   {},
	"drop":         {},
	"keep":         {},
}

// parseLokiPlan parses the supported LogQL subset.
func parseLokiPlan(raw string) (lokiPlan, error) {
	query := strings.TrimSpace(raw)
	if query == "" {
		return lokiPlan{}, fmt.Errorf("query is empty")
	}

	plan := lokiPlan{}
	idx := strings.Index(query, "count_over_time(")
	if idx >= 0 {
		metric, err := parseLokiMetricPrefix(query[:idx])
		if err != nil {
			return lokiPlan{}, err
		}
		inner, rest, err := extractBalanced(query, idx+len("count_over_time("))
		if err != nil {
			return lokiPlan{}, err
		}
		if err := finishLokiMetric(&metric, rest); err != nil {
			return lokiPlan{}, err
		}
		streamExpr, duration, err := splitLokiRange(inner)
		if err != nil {
			return lokiPlan{}, err
		}
		metric.rangeDur = duration
		plan.metric = &metric
		if err := parseLokiStreamInto(&plan, streamExpr); err != nil {
			return lokiPlan{}, err
		}
	} else if err := parseLokiStreamInto(&plan, query); err != nil {
		return lokiPlan{}, err
	}

	compileLokiPlan(&plan)
	return plan, nil
}

// finishLokiMetric consumes the closing parenthesis of the optional `sum(...)`
// wrapper plus an optional postfix grouping clause. Grafana emits both LogQL
// forms and PromQL-style postfix grouping, for example:
//
//	sum by (release) (count_over_time({service="guardian"}[1h]))
//	sum(count_over_time({service="guardian"}[1h])) by (release)
func finishLokiMetric(metric *lokiMetricPlan, rest string) error {
	rest = strings.TrimSpace(rest)
	// extractBalanced already consumed count_over_time's closing parenthesis;
	// a wrapping sum(...) leaves exactly one closing parenthesis here.
	if strings.HasPrefix(rest, ")") {
		rest = strings.TrimSpace(rest[1:])
	}
	if rest == "" {
		return nil
	}

	keyword, after := readLokiIdentifier(rest)
	switch keyword {
	case "by":
		labels, remaining, err := parseLokiGroupLabels(after)
		if err != nil {
			return err
		}
		metric.groupBy = labels
		rest = remaining
	case "without":
		return fmt.Errorf("unsupported LogQL grouping \"without\" (use \"by (...)\")")
	default:
		return fmt.Errorf("unsupported trailing query fragment %q", rest)
	}

	if trailing := strings.TrimSpace(rest); trailing != "" {
		return fmt.Errorf("unsupported trailing query fragment %q", trailing)
	}
	return nil
}

// parseLokiGroupLabels parses the parenthesised label list of a by (...) clause.
func parseLokiGroupLabels(input string) ([]string, string, error) {
	rest := strings.TrimSpace(input)
	if !strings.HasPrefix(rest, "(") {
		return nil, "", fmt.Errorf("invalid by clause: expected '('")
	}
	raw, remaining, err := extractBalanced(rest, 1)
	if err != nil {
		return nil, "", err
	}
	labels := make([]string, 0, 4)
	for _, label := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(label); trimmed != "" {
			labels = append(labels, trimmed)
		}
	}
	return labels, remaining, nil
}

func parseLokiMetricPrefix(prefix string) (lokiMetricPlan, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return lokiMetricPlan{}, nil
	}
	if !strings.HasPrefix(prefix, "sum") {
		return lokiMetricPlan{}, fmt.Errorf("unsupported LogQL metric aggregation %q", prefix)
	}
	rest := strings.TrimSpace(prefix[len("sum"):])
	metric := lokiMetricPlan{aggregate: "sum"}
	if strings.HasPrefix(rest, "by") {
		labels, after, err := parseLokiGroupLabels(rest[len("by"):])
		if err != nil {
			return lokiMetricPlan{}, fmt.Errorf("invalid sum by clause: %w", err)
		}
		metric.groupBy = labels
		rest = strings.TrimSpace(after)
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "("))
	if rest != "" {
		return lokiMetricPlan{}, fmt.Errorf("unsupported LogQL metric wrapper %q", rest)
	}
	return metric, nil
}

// splitLokiRange splits "<stream expr> [<duration>]".
func splitLokiRange(inner string) (string, time.Duration, error) {
	trimmed := strings.TrimSpace(inner)
	if !strings.HasSuffix(trimmed, "]") {
		return "", 0, fmt.Errorf("count_over_time requires a range selector")
	}
	for i := len(trimmed) - 1; i >= 0; i-- {
		if trimmed[i] != '[' {
			continue
		}
		if duration, ok := parseLokiDuration(strings.TrimSpace(trimmed[i+1 : len(trimmed)-1])); ok {
			return strings.TrimSpace(trimmed[:i]), duration, nil
		}
	}
	return "", 0, fmt.Errorf("invalid range selector in count_over_time")
}

func parseLokiDuration(value string) (time.Duration, bool) {
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
		return duration, true
	}
	// Grafana may express $__range in days or weeks.
	lower := strings.ToLower(value)
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if strings.HasSuffix(lower, suffix) {
			number, err := strconv.ParseFloat(strings.TrimSuffix(lower, suffix), 64)
			if err == nil && number > 0 {
				return time.Duration(number * float64(unit)), true
			}
		}
	}
	return 0, false
}

func parseLokiStreamInto(plan *lokiPlan, expr string) error {
	expr = strings.TrimSpace(expr)
	if !strings.HasPrefix(expr, "{") {
		return fmt.Errorf("query must start with a stream selector, got %q", expr)
	}
	end, err := findSelectorEnd(expr)
	if err != nil {
		return err
	}
	plan.selector = strings.TrimSpace(expr[:end+1])
	rest := expr[end+1:]

	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return nil
		}
		if filter, remaining, ok, err := consumeLokiLineFilter(rest); err != nil {
			return err
		} else if ok {
			plan.lineFilters = append(plan.lineFilters, filter)
			rest = remaining
			continue
		}
		if strings.HasPrefix(rest, "|") {
			rest = strings.TrimSpace(rest[1:])
			identifier, after := readLokiIdentifier(rest)
			if identifier == "" {
				return fmt.Errorf("unsupported query fragment %q", rest)
			}
			if _, isStage := lokiParserStages[identifier]; isStage {
				switch identifier {
				case "logfmt":
					plan.hasLogfmt = true
				case "json":
					plan.hasJSON = true
				case "drop", "keep":
					// drop/keep take a label list (e.g. `| drop __error__`);
					// consume the arguments so the next stage parses cleanly.
					rest = skipLokiStageArguments(after)
					continue
				}
				rest = after
				continue
			}
			// Not a parser stage: treat as a label filter stage.
			filter, remaining, err := parseLokiLabelFilter(rest)
			if err != nil {
				return err
			}
			plan.labelFilters = append(plan.labelFilters, filter)
			rest = remaining
			continue
		}
		return fmt.Errorf("unsupported query fragment %q", rest)
	}
}

func consumeLokiLineFilter(input string) (lokiLineFilter, string, bool, error) {
	for _, op := range []string{"|=", "|~", "!=", "!~"} {
		if !strings.HasPrefix(input, op) {
			continue
		}
		value, rest, err := consumeLokiQuoted(strings.TrimSpace(input[len(op):]))
		if err != nil {
			return lokiLineFilter{}, "", true, fmt.Errorf("invalid line filter: %w", err)
		}
		return lokiLineFilter{op: op, value: value}, rest, true, nil
	}
	return lokiLineFilter{}, input, false, nil
}

func parseLokiLabelFilter(input string) (lokiLabelFilter, string, error) {
	name, after := readLokiIdentifier(strings.TrimSpace(input))
	if name == "" {
		return lokiLabelFilter{}, "", fmt.Errorf("invalid label filter %q", input)
	}
	rest := strings.TrimSpace(after)
	for _, op := range []string{"=~", "!~", "!=", "="} {
		if !strings.HasPrefix(rest, op) {
			continue
		}
		value, remaining, err := consumeLokiQuoted(strings.TrimSpace(rest[len(op):]))
		if err != nil {
			return lokiLabelFilter{}, "", fmt.Errorf("invalid label filter %q: %w", input, err)
		}
		return lokiLabelFilter{name: name, op: op, value: value}, remaining, nil
	}
	return lokiLabelFilter{}, "", fmt.Errorf("invalid label filter operator in %q", input)
}

func readLokiIdentifier(input string) (string, string) {
	for i := 0; i < len(input); i++ {
		c := input[i]
		valid := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
		if !valid {
			return input[:i], input[i:]
		}
	}
	return input, ""
}

// skipLokiStageArguments drops the remainder of a parser stage's arguments up
// to the next pipeline stage. It is used for stages such as drop/keep whose
// argument grammar Doctor does not need to interpret (the labels are already
// present on the record), but whose arguments must be consumed so the following
// `|` stage is not mistaken for a label filter or an unsupported fragment.
func skipLokiStageArguments(input string) string {
	if idx := strings.Index(input, "|"); idx >= 0 {
		return input[idx:]
	}
	return ""
}

func findSelectorEnd(input string) (int, error) {
	inQuote := false
	escaped := false
	for i := 1; i < len(input); i++ {
		switch input[i] {
		case '\\':
			if inQuote && !escaped {
				escaped = true
				continue
			}
		case '"':
			if !escaped {
				inQuote = !inQuote
			}
		case '}':
			if !inQuote {
				return i, nil
			}
		}
		escaped = false
	}
	return 0, fmt.Errorf("unterminated stream selector")
}

// extractBalanced returns the contents of the paren group whose opening paren is
// at index start-1, plus the remaining input after the closing paren.
func extractBalanced(input string, start int) (string, string, error) {
	depth := 1
	inQuote := rune(0)
	escaped := false
	for i := start; i < len(input); i++ {
		r := rune(input[i])
		switch {
		case inQuote != 0:
			if r == '\\' && inQuote != '`' && !escaped {
				escaped = true
				continue
			}
			if r == inQuote && !escaped {
				inQuote = 0
			}
			escaped = false
		case r == '"' || r == '\'' || r == '`':
			inQuote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth == 0 {
				return input[start:i], input[i+1:], nil
			}
		}
	}
	return "", "", fmt.Errorf("unbalanced parentheses in query")
}

func consumeLokiQuoted(input string) (string, string, error) {
	if input == "" {
		return "", "", fmt.Errorf("expected quoted string")
	}
	quote := input[0]
	if quote != '"' && quote != '\'' && quote != '`' {
		return "", "", fmt.Errorf("expected quoted string")
	}
	escaped := false
	for i := 1; i < len(input); i++ {
		if input[i] == '\\' && quote != '`' && !escaped {
			escaped = true
			continue
		}
		if input[i] == quote && !escaped {
			raw := input[:i+1]
			if quote == '`' {
				return raw[1 : len(raw)-1], input[i+1:], nil
			}
			value, err := strconv.Unquote(raw)
			if err != nil {
				return "", "", err
			}
			return value, input[i+1:], nil
		}
		escaped = false
	}
	return "", "", fmt.Errorf("unterminated quoted string")
}

func compileLokiPlan(plan *lokiPlan) {
	for i := range plan.lineFilters {
		filter := &plan.lineFilters[i]
		if filter.op == "|~" || filter.op == "!~" {
			if re, err := regexp.Compile(filter.value); err == nil {
				filter.regex = re
			}
		}
	}
	for i := range plan.labelFilters {
		filter := &plan.labelFilters[i]
		if filter.op == "=~" || filter.op == "!~" {
			if re, err := regexp.Compile("^(?:" + filter.value + ")$"); err == nil {
				filter.regex = re
			}
		}
	}
}

// storageQuery reduces the plan to the monofs-compatible subset.
func (p lokiPlan) storageQuery() string {
	if len(p.lineFilters) == 0 {
		return p.selector
	}
	var builder strings.Builder
	builder.WriteString(p.selector)
	for _, filter := range p.lineFilters {
		builder.WriteString(" ")
		builder.WriteString(filter.op)
		builder.WriteString(" ")
		builder.WriteString(strconv.Quote(filter.value))
	}
	return builder.String()
}

func (p lokiPlan) hasStages() bool {
	return p.hasLogfmt || p.hasJSON || len(p.labelFilters) > 0 || len(p.lineFilters) > 0
}

func (p lokiPlan) applyStages(records []segment.LogRecord) []segment.LogRecord {
	if !p.hasStages() {
		return records
	}
	out := records[:0]
	for _, record := range records {
		if !p.matchLineFilters(record.Body) {
			continue
		}
		fields := p.recordFieldMap(record)
		if !p.matchLabelFilters(fields) {
			continue
		}
		out = append(out, record)
	}
	return out
}

func (p lokiPlan) matchLineFilters(body string) bool {
	for _, filter := range p.lineFilters {
		switch filter.op {
		case "|=":
			if !strings.Contains(body, filter.value) {
				return false
			}
		case "!=":
			if strings.Contains(body, filter.value) {
				return false
			}
		case "|~":
			if filter.regex == nil || !filter.regex.MatchString(body) {
				return false
			}
		case "!~":
			if filter.regex != nil && filter.regex.MatchString(body) {
				return false
			}
		}
	}
	return true
}

func (p lokiPlan) matchLabelFilters(fields map[string]string) bool {
	for _, filter := range p.labelFilters {
		value, ok := fields[filter.name]
		switch filter.op {
		case "=":
			if value != filter.value {
				return false
			}
		case "!=":
			if value == filter.value {
				return false
			}
		case "=~":
			if filter.regex == nil || !filter.regex.MatchString(value) {
				return false
			}
		case "!~":
			if filter.regex != nil && filter.regex.MatchString(value) {
				return false
			}
		}
		_ = ok
	}
	return true
}

func (p lokiPlan) recordFieldMap(record segment.LogRecord) map[string]string {
	fields := lokiLabelsFromRecord(record)
	if p.hasJSON {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(record.Body), &decoded); err == nil {
			for key, value := range decoded {
				fields[key] = stringifyLokiValue(value)
			}
		}
	}
	if p.hasLogfmt {
		parseLogfmtInto(record.Body, fields)
	}
	return fields
}

func parseLogfmtInto(body string, fields map[string]string) {
	length := len(body)
	i := 0
	for i < length {
		for i < length && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n') {
			i++
		}
		start := i
		for i < length && body[i] != '=' && body[i] != ' ' && body[i] != '\t' && body[i] != '\n' {
			i++
		}
		key := body[start:i]
		if key == "" || i >= length || body[i] != '=' {
			continue
		}
		i++ // skip '='
		var value string
		if i < length && body[i] == '"' {
			value, i = readLogfmtQuoted(body, i)
		} else {
			valueStart := i
			for i < length && body[i] != ' ' && body[i] != '\t' && body[i] != '\n' {
				i++
			}
			value = body[valueStart:i]
		}
		if key != "" {
			fields[key] = value
		}
	}
}

func readLogfmtQuoted(body string, start int) (string, int) {
	i := start + 1
	escaped := false
	for i < len(body) {
		if body[i] == '\\' && !escaped {
			escaped = true
			i++
			continue
		}
		if body[i] == '"' && !escaped {
			raw := body[start : i+1]
			value, err := strconv.Unquote(raw)
			if err != nil {
				return raw[1 : len(raw)-1], i + 1
			}
			return value, i + 1
		}
		escaped = false
		i++
	}
	return body[start+1:], len(body)
}

func stringifyLokiValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return string(encoded)
	}
}

type lokiMatrixSeries struct {
	Metric  map[string]string `json:"metric"`
	Values  [][]any           `json:"values"`
	records []time.Time
}

type lokiVectorSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

func (p lokiPlan) groupIdentity(fields map[string]string, record segment.LogRecord) (string, map[string]string) {
	if p.metric != nil && len(p.metric.groupBy) > 0 {
		labels := make(map[string]string, len(p.metric.groupBy))
		for _, name := range p.metric.groupBy {
			labels[name] = fields[name]
		}
		return metricLabelMapString(labels), labels
	}
	if p.metric != nil && p.metric.aggregate != "" {
		return "", map[string]string{}
	}
	labels := lokiLabelsFromRecord(record)
	return metricLabelMapString(labels), labels
}

func (p lokiPlan) evaluateRange(records []segment.LogRecord, start, end time.Time, step time.Duration) []lokiMatrixSeries {
	if step <= 0 {
		step = time.Minute
	}
	window := time.Duration(0)
	if p.metric != nil {
		window = p.metric.rangeDur
	}
	if window <= 0 {
		window = end.Sub(start)
	}

	groups := make(map[string]*lokiMatrixSeries)
	order := make([]string, 0)
	for _, record := range records {
		fields := p.recordFieldMap(record)
		key, labels := p.groupIdentity(fields, record)
		series := groups[key]
		if series == nil {
			series = &lokiMatrixSeries{Metric: labels}
			groups[key] = series
			order = append(order, key)
		}
		series.records = append(series.records, record.Timestamp)
	}

	out := make([]lokiMatrixSeries, 0, len(groups))
	for _, key := range order {
		series := groups[key]
		sort.Slice(series.records, func(i, j int) bool { return series.records[i].Before(series.records[j]) })
		for t := start; !t.After(end); t = t.Add(step) {
			from := t.Add(-window)
			count := 0
			for _, ts := range series.records {
				if ts.After(t) {
					break
				}
				if ts.After(from) {
					count++
				}
			}
			series.Values = append(series.Values, []any{float64(t.UnixMilli()) / 1000, strconv.Itoa(count)})
		}
		series.records = nil
		out = append(out, *series)
	}
	sort.Slice(out, func(i, j int) bool { return metricLabelMapString(out[i].Metric) < metricLabelMapString(out[j].Metric) })
	return out
}

func (p lokiPlan) evaluateInstant(records []segment.LogRecord, at time.Time) []lokiVectorSeries {
	window := time.Duration(0)
	if p.metric != nil {
		window = p.metric.rangeDur
	}
	if window <= 0 {
		window = time.Hour
	}
	from := at.Add(-window)

	counts := make(map[string]int)
	labelsByKey := make(map[string]map[string]string)
	order := make([]string, 0)
	for _, record := range records {
		if record.Timestamp.After(at) || !record.Timestamp.After(from) {
			continue
		}
		fields := p.recordFieldMap(record)
		key, labels := p.groupIdentity(fields, record)
		if _, ok := counts[key]; !ok {
			order = append(order, key)
			labelsByKey[key] = labels
		}
		counts[key]++
	}

	out := make([]lokiVectorSeries, 0, len(order))
	for _, key := range order {
		out = append(out, lokiVectorSeries{
			Metric: labelsByKey[key],
			Value:  []any{float64(at.UnixMilli()) / 1000, strconv.Itoa(counts[key])},
		})
	}
	sort.Slice(out, func(i, j int) bool { return metricLabelMapString(out[i].Metric) < metricLabelMapString(out[j].Metric) })
	return out
}

func parseLokiStep(value string, start, end time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value != "" {
		if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
			return duration
		}
		if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	span := end.Sub(start)
	if span <= 0 {
		return time.Minute
	}
	step := span / 100
	if step < time.Second {
		step = time.Second
	}
	return step
}
