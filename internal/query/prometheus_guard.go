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

// rejectBroadRangeSelector rejects a range query that reads every series (for
// example {__name__=~".+"}, including the same selector wrapped in rate/sum)
// before the PromQL engine starts loading samples. Count presence aggregations
// are handled by the series-index fast path before this guard runs.
func rejectBroadRangeSelector(expr promparser.Expr, start, end time.Time) error {
	if end.Sub(start) <= maxBroadSelectorRange {
		return nil
	}
	if findBroadSelector(expr) == nil {
		return nil
	}
	return fmt.Errorf("%w: {__name__=~\".+\"} selects every series over %s; narrow the range to %s or less, use the metric picker, or aggregate with count by (__name__)",
		errBroadRangeSelector, end.Sub(start), maxBroadSelectorRange)
}

func findBroadSelector(expr promparser.Expr) promparser.Expr {
	switch typed := expr.(type) {
	case *promparser.VectorSelector:
		if len(typed.LabelMatchers) != 1 {
			return nil
		}
		matcher := typed.LabelMatchers[0]
		if matcher == nil || matcher.Name != promlabels.MetricName || !matcherMatchesAllMetricNames(matcher) {
			return nil
		}
		return typed
	case *promparser.AggregateExpr:
		return findBroadSelector(typed.Expr)
	case *promparser.Call:
		for _, arg := range typed.Args {
			if found := findBroadSelector(arg); found != nil {
				return found
			}
		}
	case *promparser.MatrixSelector:
		return findBroadSelector(typed.VectorSelector)
	case *promparser.SubqueryExpr:
		return findBroadSelector(typed.Expr)
	case *promparser.ParenExpr:
		return findBroadSelector(typed.Expr)
	case *promparser.UnaryExpr:
		return findBroadSelector(typed.Expr)
	case *promparser.StepInvariantExpr:
		return findBroadSelector(typed.Expr)
	case *promparser.BinaryExpr:
		if found := findBroadSelector(typed.LHS); found != nil {
			return found
		}
		return findBroadSelector(typed.RHS)
	}
	return nil
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
