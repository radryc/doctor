package alerting

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Rule authoring modes.
const (
	ModeGuided = "guided"
	ModePromQL = "promql"
)

// Golden Signal kinds. These are generic compiler shapes, not alert templates:
// every alert is a parameterised instance of one of them.
const (
	SignalErrors       = "errors"
	SignalLatency      = "latency"
	SignalTraffic      = "traffic"
	SignalSaturation   = "saturation"
	SignalAvailability = "availability"
)

var (
	metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNamePattern  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// AlertSpec is the structured, non-PromQL definition of a guided alert.
// Sentinel compiles it to PromQL on save and stores both.
type AlertSpec struct {
	Signal        string       `json:"signal" yaml:"signal"`
	Primary       SignalQuery  `json:"primary" yaml:"primary"`
	Denominator   *SignalQuery `json:"denominator,omitempty" yaml:"denominator,omitempty"`
	Condition     Condition    `json:"condition" yaml:"condition"`
	Window        Duration     `json:"window,omitempty" yaml:"window,omitempty"`
	For           Duration     `json:"for,omitempty" yaml:"for,omitempty"`
	KeepFiringFor Duration     `json:"keepFiringFor,omitempty" yaml:"keepFiringFor,omitempty"`
	Severity      string       `json:"severity,omitempty" yaml:"severity,omitempty"`
	Summary       string       `json:"summary,omitempty" yaml:"summary,omitempty"`
	RunbookURL    string       `json:"runbookUrl,omitempty" yaml:"runbookUrl,omitempty"`
	Destinations  []string     `json:"destinations,omitempty" yaml:"destinations,omitempty"`
}

// SignalQuery describes one metric stream.
type SignalQuery struct {
	Metric   string    `json:"metric" yaml:"metric"`
	Filters  []Matcher `json:"filters,omitempty" yaml:"filters,omitempty"`
	Function string    `json:"function,omitempty" yaml:"function,omitempty"` // rate|increase|avg_over_time|max_over_time|min_over_time|none
	Reduce   string    `json:"reduce,omitempty" yaml:"reduce,omitempty"`     // sum|avg|max|min|count|none
	Quantile float64   `json:"quantile,omitempty" yaml:"quantile,omitempty"`
	By       []string  `json:"by,omitempty" yaml:"by,omitempty"`
}

// Condition is the threshold comparison.
type Condition struct {
	Op    string  `json:"op" yaml:"op"` // gt|gte|lt|lte|eq|neq|absent
	Value float64 `json:"value" yaml:"value"`
	Unit  string  `json:"unit,omitempty" yaml:"unit,omitempty"` // percent|ms|s|us|rps|bytes|millicores|seconds
}

// Validate checks a guided specification.
func (s *AlertSpec) Validate() error {
	if s == nil {
		return fmt.Errorf("alerting: alert spec is nil")
	}
	switch s.Signal {
	case SignalErrors, SignalLatency, SignalTraffic, SignalSaturation, SignalAvailability:
	default:
		return fmt.Errorf("alerting: unsupported signal %q", s.Signal)
	}
	if err := s.Primary.Validate(s.WindowOrDefault()); err != nil {
		return fmt.Errorf("alerting: primary signal: %w", err)
	}
	if s.Denominator != nil {
		if err := s.Denominator.Validate(s.WindowOrDefault()); err != nil {
			return fmt.Errorf("alerting: denominator signal: %w", err)
		}
	}
	switch s.Condition.Op {
	case "gt", ">", "gte", ">=", "lt", "<", "lte", "<=", "eq", "==", "neq", "!=", "absent":
	default:
		return fmt.Errorf("alerting: unsupported condition %q", s.Condition.Op)
	}
	if s.Signal != SignalAvailability && s.Condition.Op == "absent" {
		return fmt.Errorf("alerting: only availability alerts may use the absent condition")
	}
	if s.Window < 0 || s.For < 0 || s.KeepFiringFor < 0 {
		return fmt.Errorf("alerting: durations must not be negative")
	}
	if s.Window > 0 && time.Duration(s.Window) < 10*time.Second {
		return fmt.Errorf("alerting: window must be at least 10s")
	}
	if s.Window > 0 && time.Duration(s.Window) > 24*time.Hour {
		return fmt.Errorf("alerting: window must be at most 24h")
	}
	if s.Severity == "" {
		s.Severity = "warning"
	}
	return nil
}

// Validate checks one signal query.
func (q *SignalQuery) Validate(window time.Duration) error {
	if !metricNamePattern.MatchString(strings.TrimSpace(q.Metric)) {
		return fmt.Errorf("invalid metric name %q", q.Metric)
	}
	for _, f := range q.Filters {
		if !labelNamePattern.MatchString(strings.TrimSpace(f.Name)) {
			return fmt.Errorf("invalid label name %q", f.Name)
		}
		switch strings.TrimSpace(f.Operator) {
		case "", "=", "==", "!=", "=~", "!~":
		default:
			return fmt.Errorf("invalid matcher operator %q", f.Operator)
		}
	}
	switch strings.TrimSpace(q.Function) {
	case "", "none", "rate", "increase", "avg_over_time", "max_over_time", "min_over_time":
	default:
		return fmt.Errorf("invalid function %q", q.Function)
	}
	switch strings.TrimSpace(q.Reduce) {
	case "", "none", "sum", "avg", "max", "min", "count":
	default:
		return fmt.Errorf("invalid reducer %q", q.Reduce)
	}
	for _, label := range q.By {
		if !labelNamePattern.MatchString(strings.TrimSpace(label)) {
			return fmt.Errorf("invalid group-by label %q", label)
		}
	}
	if q.Quantile < 0 || q.Quantile > 1 {
		return fmt.Errorf("quantile must be between 0 and 1")
	}
	if window <= 0 {
		return nil
	}
	if q.Function == "rate" || q.Function == "increase" ||
		q.Function == "avg_over_time" || q.Function == "max_over_time" || q.Function == "min_over_time" {
		if time.Duration(window) < 10*time.Second {
			return fmt.Errorf("range function requires a window of at least 10s")
		}
	}
	return nil
}

// WindowOrDefault returns the look-back window.
func (s *AlertSpec) WindowOrDefault() time.Duration {
	if s.Window > 0 {
		return time.Duration(s.Window)
	}
	return 5 * time.Minute
}

// CompileSpec turns a guided specification into PromQL. This is the only place
// where guided alerts generate PromQL.
func CompileSpec(spec *AlertSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	window := formatDuration(spec.WindowOrDefault())
	op, err := comparison(spec.Condition.Op)
	if err != nil {
		return "", err
	}
	value := formatNumber(spec.Condition.Value)
	primary, err := compileQuery(spec.Primary, window, defaultFunction(spec.Signal), defaultReducer(spec.Signal))
	if err != nil {
		return "", err
	}
	denominator := ""
	if spec.Denominator != nil {
		denominator, err = compileQuery(*spec.Denominator, window, defaultFunction(spec.Signal), defaultReducer(spec.Signal))
		if err != nil {
			return "", err
		}
	}

	switch spec.Signal {
	case SignalErrors:
		if denominator != "" {
			return fmt.Sprintf("100 * (%s / clamp_min(%s, 0.001)) %s %s", primary, denominator, op, value), nil
		}
		return fmt.Sprintf("%s %s %s", primary, op, value), nil

	case SignalLatency:
		if !strings.HasSuffix(spec.Primary.Metric, "_bucket") {
			return "", fmt.Errorf("alerting: latency alerts require a histogram bucket metric ending in _bucket")
		}
		quantile := spec.Primary.Quantile
		if quantile <= 0 || quantile >= 1 {
			quantile = 0.95
		}
		by := append([]string{"le"}, cleanLabels(spec.Primary.By)...)
		aggregate := fmt.Sprintf("sum by (%s) (rate(%s[%s]))", strings.Join(by, ", "), metricSelector(spec.Primary), window)
		expr := fmt.Sprintf("histogram_quantile(%s, %s)", formatNumber(quantile), aggregate)
		if scale, ok := latencyScale(spec.Condition.Unit); ok {
			expr = fmt.Sprintf("%s * %s", expr, formatNumber(scale))
		}
		return fmt.Sprintf("%s %s %s", expr, op, value), nil

	case SignalTraffic:
		return fmt.Sprintf("%s %s %s", primary, op, value), nil

	case SignalSaturation:
		if denominator != "" {
			if strings.EqualFold(spec.Condition.Unit, "percent") || spec.Condition.Unit == "" {
				return fmt.Sprintf("100 * (%s / clamp_min(%s, 0.001)) %s %s", primary, denominator, op, value), nil
			}
			return fmt.Sprintf("%s / clamp_min(%s, 0.001) %s %s", primary, denominator, op, value), nil
		}
		return fmt.Sprintf("%s %s %s", primary, op, value), nil

	case SignalAvailability:
		if spec.Condition.Op == "absent" {
			return fmt.Sprintf("absent(%s)", metricSelector(spec.Primary)), nil
		}
		if denominator != "" {
			return fmt.Sprintf("100 * (%s / clamp_min(%s, 0.001)) %s %s", primary, denominator, op, value), nil
		}
		return fmt.Sprintf("%s %s %s", primary, op, value), nil
	}
	return "", fmt.Errorf("alerting: unsupported signal %q", spec.Signal)
}

func defaultFunction(signal string) string {
	switch signal {
	case SignalErrors, SignalTraffic:
		return "rate"
	case SignalAvailability:
		return "none"
	default:
		return "none"
	}
}

func defaultReducer(signal string) string {
	switch signal {
	case SignalTraffic, SignalErrors:
		return "sum"
	case SignalAvailability:
		return "min"
	default:
		return "avg"
	}
}

func compileQuery(q SignalQuery, window, fallbackFunction, fallbackReduce string) (string, error) {
	function := strings.TrimSpace(q.Function)
	if function == "" {
		function = fallbackFunction
	}
	if function == "" {
		function = "none"
	}
	reduce := strings.TrimSpace(q.Reduce)
	if reduce == "" {
		reduce = fallbackReduce
	}
	if reduce == "" {
		reduce = "sum"
	}
	selector := metricSelector(q)
	inner := selector
	switch function {
	case "none":
	case "rate", "increase", "avg_over_time", "max_over_time", "min_over_time":
		inner = fmt.Sprintf("%s(%s[%s])", function, selector, window)
	default:
		return "", fmt.Errorf("alerting: unsupported function %q", function)
	}
	if reduce == "none" {
		return inner, nil
	}
	if len(q.By) > 0 {
		return fmt.Sprintf("%s by (%s) (%s)", reduce, strings.Join(cleanLabels(q.By), ", "), inner), nil
	}
	return fmt.Sprintf("%s(%s)", reduce, inner), nil
}

func metricSelector(q SignalQuery) string {
	if len(q.Filters) == 0 {
		return q.Metric
	}
	parts := make([]string, 0, len(q.Filters))
	for _, f := range q.Filters {
		op := strings.TrimSpace(f.Operator)
		switch op {
		case "", "==":
			op = "="
		}
		parts = append(parts, fmt.Sprintf(`%s%s"%s"`, strings.TrimSpace(f.Name), op, escapeLabelValue(f.Value)))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%s{%s}", q.Metric, strings.Join(parts, ","))
}

func escapeLabelValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func cleanLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func comparison(op string) (string, error) {
	switch strings.TrimSpace(op) {
	case "gt", ">":
		return ">", nil
	case "gte", ">=":
		return ">=", nil
	case "lt", "<":
		return "<", nil
	case "lte", "<=":
		return "<=", nil
	case "eq", "==":
		return "==", nil
	case "neq", "!=":
		return "!=", nil
	case "absent":
		return "", nil
	default:
		return "", fmt.Errorf("alerting: unsupported condition %q", op)
	}
}

func latencyScale(unit string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "ms":
		return 1000, true
	case "s", "sec", "seconds":
		return 1, true
	case "us", "µs", "microseconds":
		return 1_000_000, true
	default:
		return 0, false
	}
}

func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func formatDuration(value time.Duration) string {
	if value <= 0 {
		return "0s"
	}
	if value%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(value.Hours()))
	}
	if value%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(value.Minutes()))
	}
	if value%time.Second == 0 {
		return fmt.Sprintf("%ds", int(value.Seconds()))
	}
	return value.String()
}
