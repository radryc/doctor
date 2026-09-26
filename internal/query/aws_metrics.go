package query

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql"
)

// This file federates CloudWatch metrics into the Prometheus API using
// CloudWatch's Prometheus-compatible endpoints (SigV4-signed via the AWS
// pusher). Unlike a selector translation this preserves the full PromQL
// language, including functions, aggregations and histogram_quantile.

const (
	awsQueryNone = iota
	awsQueryOnly
	awsQueryMixed
)

var (
	awsSourceExactRe  = regexp.MustCompile(`\bsource\s*=\s*"aws"`)
	awsSourceRegexRe  = regexp.MustCompile(`\bsource\s*=~\s*"([^"]*)"`)
	selectorLeadComma = regexp.MustCompile(`\{\s*,`)
	selectorTailComma = regexp.MustCompile(`,\s*\}`)
	selectorLeadSpace = regexp.MustCompile(`\{\s+`)
	selectorTailSpace = regexp.MustCompile(`\s+\}`)
)

// awsQueryMode classifies a query and returns it with the source matcher removed
// (CloudWatch has no `source` label).
func awsQueryMode(query string) (string, int) {
	if awsSourceExactRe.MatchString(query) {
		return stripAWSSourceMatcher(query), awsQueryOnly
	}
	if match := awsSourceRegexRe.FindStringSubmatch(query); len(match) == 2 {
		value := match[1]
		if strings.Contains(value, "aws") {
			mode := awsQueryMixed
			if value == "aws" {
				mode = awsQueryOnly
			}
			return stripAWSSourceMatcher(query), mode
		}
	}
	return query, awsQueryNone
}

func stripAWSSourceMatcher(query string) string {
	out := awsSourceExactRe.ReplaceAllString(query, "")
	out = awsSourceRegexRe.ReplaceAllString(out, "")
	out = selectorLeadComma.ReplaceAllString(out, "{")
	out = selectorTailComma.ReplaceAllString(out, "}")
	out = strings.ReplaceAll(out, ",,", ",")
	out = selectorLeadSpace.ReplaceAllString(out, "{")
	out = selectorTailSpace.ReplaceAllString(out, "}")
	// A metric selector that becomes empty should collapse to the bare metric
	// name (CloudWatch accepts `Metric` but not `Metric{}`).
	out = strings.ReplaceAll(out, "{}", "")
	return out
}

// awsMatches splits match[] selectors into whether any targets AWS and the
// AWS-forwardable (source-stripped) selectors.
func awsMatches(matches []string) (bool, []string) {
	hasAWS := false
	forwarded := make([]string, 0, len(matches))
	for _, match := range matches {
		stripped, mode := awsQueryMode(match)
		if mode != awsQueryNone {
			hasAWS = true
			forwarded = append(forwarded, stripped)
		} else {
			forwarded = append(forwarded, match)
		}
	}
	return hasAWS, forwarded
}

func extractPrometheusData(response map[string]any) map[string]any {
	if response == nil {
		return nil
	}
	data, _ := response["data"].(map[string]any)
	return data
}

// mergePrometheusQueryData merges CloudWatch vector/matrix data into the local
// result, tagging AWS series with source="aws".
func mergePrometheusQueryData(local, aws map[string]any, defaultType string) map[string]any {
	if local == nil && aws == nil {
		return map[string]any{"resultType": defaultType, "result": []any{}}
	}
	if local == nil {
		return tagPrometheusData(aws)
	}
	if aws == nil {
		return local
	}
	localType, _ := local["resultType"].(string)
	awsType, _ := aws["resultType"].(string)
	if localType != "" && awsType != "" && localType != awsType {
		return local
	}
	localResult, _ := local["result"].([]any)
	awsResult, _ := aws["result"].([]any)
	for _, item := range awsResult {
		localResult = append(localResult, tagPrometheusItem(item))
	}
	local["result"] = localResult
	return local
}

func tagPrometheusData(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	if result, ok := data["result"].([]any); ok {
		tagged := make([]any, 0, len(result))
		for _, item := range result {
			tagged = append(tagged, tagPrometheusItem(item))
		}
		data["result"] = tagged
	}
	return data
}

func tagPrometheusItem(item any) any {
	m, ok := item.(map[string]any)
	if !ok {
		return item
	}
	if metric, ok := m["metric"].(map[string]any); ok {
		if _, exists := metric[labelSource]; !exists {
			metric[labelSource] = awsSourceLabel
		}
		m["metric"] = metric
		return m
	}
	// series-style item: the map itself is the label set
	if _, exists := m[labelSource]; !exists {
		m[labelSource] = awsSourceLabel
	}
	return m
}

func prometheusDataStrings(data map[string]any) []string {
	if data == nil {
		return nil
	}
	values, _ := data["result"].([]any)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if str, ok := value.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

func prometheusDataLabelMaps(data map[string]any) []map[string]string {
	if data == nil {
		return nil
	}
	values, _ := data["result"].([]any)
	out := make([]map[string]string, 0, len(values))
	for _, value := range values {
		m, ok := value.(map[string]any)
		if !ok {
			continue
		}
		labels := make(map[string]string, len(m)+1)
		for key, raw := range m {
			if str, ok := raw.(string); ok {
				labels[key] = str
			}
		}
		labels[labelSource] = awsSourceLabel
		out = append(out, labels)
	}
	return out
}

// execPrometheusInstant runs an instant query against local storage. parseErr is
// true when the query failed to parse (HTTP 400) rather than at execution time.
func (s *Service) execPrometheusInstant(ctx context.Context, tenant, queryString string, at time.Time) (map[string]any, error, bool) {
	engine := newDoctorPrometheusEngine()
	defer func() { _ = engine.Close() }()

	query, err := engine.NewInstantQuery(
		ctx,
		doctorPromQueryable{backend: s.logEngine, tenant: tenant},
		promql.NewPrometheusQueryOpts(false, doctorPrometheusLookback),
		queryString,
		at,
	)
	if err != nil {
		return nil, err, true
	}
	defer query.Close()

	result := query.Exec(ctx)
	if result.Err != nil {
		return nil, result.Err, false
	}
	data, err := marshalPrometheusValue(result.Value)
	if err != nil {
		return nil, err, false
	}
	return data, nil, false
}

// execPrometheusRange runs a range query against local storage.
func (s *Service) execPrometheusRange(ctx context.Context, tenant, queryString string, start, end time.Time, step time.Duration) (map[string]any, error, bool) {
	engine := newDoctorPrometheusEngine()
	defer func() { _ = engine.Close() }()

	query, err := engine.NewRangeQuery(
		ctx,
		doctorPromQueryable{backend: s.logEngine, tenant: tenant},
		promql.NewPrometheusQueryOpts(false, doctorPrometheusLookback),
		queryString,
		start,
		end,
		step,
	)
	if err != nil {
		return nil, err, true
	}
	defer query.Close()

	result := query.Exec(ctx)
	if result.Err != nil {
		return nil, result.Err, false
	}
	data, err := marshalPrometheusValue(result.Value)
	if err != nil {
		return nil, err, false
	}
	return data, nil, false
}

func sortedStrings(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
