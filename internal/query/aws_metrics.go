package query

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"
	"github.com/radryc/doctor/internal/proxy"
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
		labels, source := prometheusSeriesLabels(entry)
		value := firstNonEmpty(
			labels["guardian_partition"],
			labels["@aws.tag.guardian.io/partition"],
			labels["@aws.tag.strata.io/partition"],
			labels["partition"],
		)
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

// filterPrometheusDataByLabel keeps only series matching one canonical
// identity label (intent or asset). Unlike partition, AWS dimension-only
// series are not exempt: if they carry no identity they cannot match.
func filterPrometheusDataByLabel(data map[string]any, value string, keys ...string) map[string]any {
	value = strings.TrimSpace(value)
	if data == nil || value == "" {
		return data
	}
	result := proxy.PrometheusResultSlice(data)
	filtered := make([]any, 0, len(result))
	for _, item := range result {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		labels, _ := prometheusSeriesLabels(entry)
		for _, key := range keys {
			if labels[key] == value {
				filtered = append(filtered, item)
				break
			}
		}
	}
	data["result"] = filtered
	return data
}

var (
	prometheusIntentLabels = []string{"guardian_intent", "@aws.tag.guardian.io/intent", "@aws.tag.strata.io/intent", "intent"}
	prometheusAssetLabels  = []string{"guardian_asset", "@aws.tag.guardian.io/asset", "@aws.tag.strata.io/asset", "asset"}
)

// filterPrometheusDataByIdentity applies the UI's partition/intent/asset
// filters to a merged Prometheus result. It runs for local, federated, and
// router-backed queries so every signal uses the same identity fields.
func filterPrometheusDataByIdentity(r *http.Request, data map[string]any) map[string]any {
	identity := identityFromRequest(r)
	data = filterPrometheusDataByPartition(data, identity.Partition)
	data = filterPrometheusDataByLabel(data, identity.Intent, prometheusIntentLabels...)
	data = filterPrometheusDataByLabel(data, identity.Asset, prometheusAssetLabels...)
	return data
}

// prometheusSeriesLabels normalizes the two result-map shapes produced by the
// local engine and federated backends.
func prometheusSeriesLabels(entry map[string]any) (map[string]string, string) {
	switch metric := entry["metric"].(type) {
	case map[string]string:
		return metric, metric["source"]
	case map[string]any:
		labels := make(map[string]string, len(metric))
		for key, candidate := range metric {
			if value, ok := candidate.(string); ok {
				labels[key] = value
			}
		}
		return labels, labels["source"]
	default:
		return map[string]string{}, ""
	}
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
