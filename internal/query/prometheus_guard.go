package query

import (
	"errors"
	"fmt"
	"time"

	promlabels "github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

// maxBroadSelectorRange bounds a bare selector that matches every metric name.
// A range query over every series has to read every sample in the window; the
// result is usually a dashboard mistake, not a useful graph.
const maxBroadSelectorRange = 15 * time.Minute

var errBroadRangeSelector = errors.New("selector matches all metric series")

// rejectBroadRangeSelector rejects a bare {__name__=~".+"}-style range query
// before the PromQL engine starts loading samples. Aggregations handled by the
// count fast path (count and count by (__name__)) are intentionally allowed.
func rejectBroadRangeSelector(expr promparser.Expr, start, end time.Time) error {
	if end.Sub(start) <= maxBroadSelectorRange {
		return nil
	}
	selector, ok := expr.(*promparser.VectorSelector)
	if !ok || len(selector.LabelMatchers) != 1 {
		return nil
	}
	matcher := selector.LabelMatchers[0]
	if matcher == nil || matcher.Name != promlabels.MetricName || !matcherMatchesAllMetricNames(matcher) {
		return nil
	}
	return fmt.Errorf("%w: {__name__=~\".+\"} selects every series over %s; narrow the range to %s or less, use the metric picker, or aggregate with count by (__name__)",
		errBroadRangeSelector, end.Sub(start), maxBroadSelectorRange)
}

func matcherMatchesAllMetricNames(matcher *promlabels.Matcher) bool {
	switch matcher.Type {
	case promlabels.MatchEqual:
		return false
	case promlabels.MatchNotEqual:
		return matcher.Value == ""
	case promlabels.MatchRegexp:
		return allMetricNameRegex(matcher.Value)
	case promlabels.MatchNotRegexp:
		return matcher.Value == "" || matcher.Value == "^$"
	default:
		return false
	}
}

func allMetricNameRegex(value string) bool {
	switch value {
	case "", ".+", ".*", "^.+$", "^.*$", "(?s).+", "(?s).*":
		return true
	default:
		return false
	}
}
