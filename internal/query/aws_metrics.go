package query

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"
	"github.com/rydzu/ainfra/doctor/internal/proxy"
)

// This file federates CloudWatch metrics into the Prometheus API using
// CloudWatch's Prometheus-compatible endpoints (SigV4-signed via the AWS
// pusher). The query classification and result merging live in internal/proxy;
// the wrappers below keep the handler call sites and tests stable while the
// multi-backend router is built out.

const (
	awsQueryNone  = proxy.AWSQueryNone
	awsQueryOnly  = proxy.AWSQueryOnly
	awsQueryMixed = proxy.AWSQueryMixed
)

// awsQueryMode classifies a query and returns it with the source matcher removed.
func awsQueryMode(query string) (string, int) { return proxy.AWSQueryMode(query) }

// awsMatches splits match[] selectors into whether any targets AWS and the
// AWS-forwardable (source-stripped) selectors.
func awsMatches(matches []string) (bool, []string) { return proxy.AWSMatches(matches) }

// mergePrometheusQueryData merges the AWS result into the local result,
// tagging AWS series with source="aws".
func mergePrometheusQueryData(local, aws map[string]any, defaultType string) map[string]any {
	return proxy.MergePrometheusQueryData(local, defaultType, proxy.PromRemote{Source: proxy.SourceAWS, Data: aws})
}

// filterPrometheusDataByPartition applies the UI partition filter to the final
// merged result, including federated AWS series that are not served by the
// local PromQL engine.
func filterPrometheusDataByPartition(data map[string]any, partition string) map[string]any {
	partition = strings.TrimSpace(partition)
	if data == nil || partition == "" {
		return data
	}
	result := proxy.PrometheusResultSlice(data)
	filtered := make([]any, 0, len(result))
	for _, item := range result {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		value, source := "", ""
		switch metric := entry["metric"].(type) {
		case map[string]string:
			value = firstNonEmpty(
				metric["guardian_partition"],
				metric["@aws.tag.guardian.io/partition"],
				metric["@aws.tag.strata.io/partition"],
				metric["partition"],
			)
			source = metric["source"]
		case map[string]any:
			for _, key := range []string{"guardian_partition", "@aws.tag.guardian.io/partition", "@aws.tag.strata.io/partition", "partition"} {
				if candidate, ok := metric[key].(string); ok && candidate != "" {
					value = candidate
					break
				}
			}
			source, _ = metric["source"].(string)
		}
		// Dimension-only AWS series may not carry a partition tag. The AWS
		// backend is account-scoped, so keep them instead of silently dropping
		// every CloudWatch metric when a partition filter is active.
		if value == partition || (value == "" && source == proxy.SourceAWS) {
			filtered = append(filtered, item)
		}
	}
	data["result"] = filtered
	return data
}

func extractPrometheusData(response map[string]any) map[string]any {
	return proxy.ExtractPrometheusData(response)
}

func extractPrometheusStringList(response map[string]any) []string {
	return proxy.ExtractPrometheusStringList(response)
}

func extractPrometheusSeries(response map[string]any) []map[string]string {
	return proxy.ExtractPrometheusSeries(response)
}

func prometheusResultEmpty(data map[string]any) bool {
	return proxy.PrometheusResultEmpty(data)
}

// execPrometheusInstant runs an instant query against local storage. parseErr is
// true when the query failed to parse (HTTP 400) rather than at execution time.
// execPrometheusInstant runs an instant query against local storage.
func (s *Service) execPrometheusInstant(ctx context.Context, tenant, partition, queryString string, at time.Time) (map[string]any, error, bool) {
	// Presence aggregations (count by (__name__)) are answered from the chunk
	// series index instead of scanning samples.
	if expr, parseErr := promparser.NewParser(promparser.Options{}).ParseExpr(queryString); parseErr == nil {
		if plan, ok := countByNamePlanFor(expr); ok {
			data, fastErr := s.execCountByNameInstant(ctx, tenant, partition, plan, at)
			if fastErr != nil {
				return nil, fastErr, false
			}
			return data, nil, false
		}
	}

	engine := newDoctorPrometheusEngine(prometheusEngineTimeout(ctx))
	defer func() { _ = engine.Close() }()

	query, err := engine.NewInstantQuery(
		ctx,
		doctorPromQueryable{backend: s.logEngine, tenant: tenant, partition: partition},
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
func (s *Service) execPrometheusRange(ctx context.Context, tenant, partition, queryString string, start, end time.Time, step time.Duration) (map[string]any, error, bool) {
	// Presence aggregations (count by (__name__)) are answered from the chunk
	// series index instead of scanning samples.
	expr, parseErr := promparser.NewParser(promparser.Options{}).ParseExpr(queryString)
	if parseErr == nil {
		if plan, ok := countByNamePlanFor(expr); ok {
			data, fastErr := s.execCountByNameRange(ctx, tenant, partition, plan, start, end, step)
			if fastErr != nil {
				return nil, fastErr, false
			}
			return data, nil, false
		}
		if err := rejectBroadRangeSelector(expr, start, end); err != nil {
			return nil, err, false
		}
	}

	engine := newDoctorPrometheusEngine(prometheusEngineTimeout(ctx))
	defer func() { _ = engine.Close() }()

	query, err := engine.NewRangeQuery(
		ctx,
		doctorPromQueryable{backend: s.logEngine, tenant: tenant, partition: partition},
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
