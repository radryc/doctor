package proxy

import (
	"reflect"
	"testing"
)

func vectorItem(name string, labels map[string]string, value string) map[string]any {
	metric := map[string]any{"__name__": name}
	for key, label := range labels {
		metric[key] = label
	}
	return map[string]any{
		"metric": metric,
		"value":  []any{float64(1780000000), value},
	}
}

func TestMergePrometheusQueryDataSingleRemoteMatchesLegacyBehavior(t *testing.T) {
	local := map[string]any{
		"resultType": "vector",
		"result":     []any{vectorItem("up", map[string]string{"source": SourceDoctor}, "1")},
	}
	remote := map[string]any{
		"resultType": "vector",
		"result":     []any{vectorItem("CPUUtilization", nil, "7")},
	}

	merged := MergePrometheusQueryData(local, "vector", PromRemote{Source: SourceAWS, Data: remote})
	result := PrometheusResultSlice(merged)
	if len(result) != 2 {
		t.Fatalf("result length = %d, want 2", len(result))
	}
	first := result[0].(map[string]any)["metric"].(map[string]any)
	if first[LabelSource] != SourceDoctor {
		t.Fatalf("local source = %v, want doctor", first[LabelSource])
	}
	if _, hasShard := first[LabelShard]; hasShard {
		t.Fatalf("local series unexpectedly got a shard label: %#v", first)
	}
	second := result[1].(map[string]any)["metric"].(map[string]any)
	if second[LabelSource] != SourceAWS {
		t.Fatalf("remote source = %v, want aws", second[LabelSource])
	}
	if _, hasShard := second[LabelShard]; hasShard {
		t.Fatalf("remote series unexpectedly got a shard label: %#v", second)
	}
}

func TestMergePrometheusQueryDataNilLocalTagsRemote(t *testing.T) {
	remote := map[string]any{
		"resultType": "vector",
		"result":     []any{vectorItem("CPUUtilization", nil, "7")},
	}
	merged := MergePrometheusQueryData(nil, "vector", PromRemote{Source: SourceAWS, Data: remote})
	if merged["resultType"] != "vector" {
		t.Fatalf("resultType = %v, want vector", merged["resultType"])
	}
	metric := PrometheusResultSlice(merged)[0].(map[string]any)["metric"].(map[string]any)
	if metric[LabelSource] != SourceAWS {
		t.Fatalf("source = %v, want aws", metric[LabelSource])
	}
}

func TestMergePrometheusQueryDataBothNilReturnsEmptyDefault(t *testing.T) {
	merged := MergePrometheusQueryData(nil, "matrix", PromRemote{Source: SourceAWS, Data: nil})
	if merged["resultType"] != "matrix" {
		t.Fatalf("resultType = %v, want matrix", merged["resultType"])
	}
	if len(PrometheusResultSlice(merged)) != 0 {
		t.Fatalf("result = %#v, want empty", merged["result"])
	}
}

func TestMergePrometheusQueryDataTypeMismatchKeepsLocal(t *testing.T) {
	local := map[string]any{
		"resultType": "vector",
		"result":     []any{vectorItem("up", nil, "1")},
	}
	remote := map[string]any{
		"resultType": "matrix",
		"result":     []any{},
	}
	merged := MergePrometheusQueryData(local, "vector", PromRemote{Source: SourceAWS, Data: remote})
	if merged["resultType"] != "vector" {
		t.Fatalf("resultType = %v, want vector", merged["resultType"])
	}
	if len(PrometheusResultSlice(merged)) != 1 {
		t.Fatalf("result = %#v, want only the local series", merged["result"])
	}
}

func TestMergePrometheusQueryDataAddsShardOnOverlap(t *testing.T) {
	series := func(source string) map[string]any {
		return map[string]any{
			"resultType": "vector",
			"result":     []any{vectorItem("up", map[string]string{"instance": "x", LabelSource: source}, "1")},
		}
	}
	// Two shards of the same kind return the same series: both are tagged
	// source=prometheus, so they collide and get shard labels. The native
	// doctor series is distinguished by source=doctor and stays unsharded.
	merged := MergePrometheusQueryData(
		series(SourceDoctor),
		"vector",
		PromRemote{Source: "prometheus", Backend: "prom-a", Data: series("prometheus")},
		PromRemote{Source: "prometheus", Backend: "prom-b", Data: series("prometheus")},
	)
	result := PrometheusResultSlice(merged)
	if len(result) != 3 {
		t.Fatalf("result length = %d, want 3", len(result))
	}
	shards := map[string]int{}
	for _, item := range result {
		metric := item.(map[string]any)["metric"].(map[string]any)
		if shard, ok := metric[LabelShard].(string); ok {
			shards[shard]++
		}
	}
	if len(shards) != 2 || shards["prom-a"] != 1 || shards["prom-b"] != 1 {
		t.Fatalf("shards = %#v, want prom-a and prom-b once each", shards)
	}
	localMetric := result[0].(map[string]any)["metric"].(map[string]any)
	if _, hasShard := localMetric[LabelShard]; hasShard {
		t.Fatalf("native series unexpectedly sharded: %#v", localMetric)
	}
}

func TestMergePrometheusQueryDataSameSourceDuplicatesNotSharded(t *testing.T) {
	series := map[string]any{
		"resultType": "vector",
		"result": []any{
			vectorItem("up", map[string]string{"instance": "x", LabelSource: SourceAWS}, "1"),
			vectorItem("up", map[string]string{"instance": "x", LabelSource: SourceAWS}, "1"),
		},
	}
	merged := MergePrometheusQueryData(nil, "vector", PromRemote{Source: SourceAWS, Data: series})
	for _, item := range PrometheusResultSlice(merged) {
		metric := item.(map[string]any)["metric"].(map[string]any)
		if _, hasShard := metric[LabelShard]; hasShard {
			t.Fatalf("same-source duplicate was sharded: %#v", metric)
		}
	}
}

func TestExtractPrometheusStringList(t *testing.T) {
	bare := ExtractPrometheusStringList(map[string]any{"data": []any{"a", "b"}})
	if !reflect.DeepEqual(bare, []string{"a", "b"}) {
		t.Fatalf("bare = %#v", bare)
	}
	wrapped := ExtractPrometheusStringList(map[string]any{"data": map[string]any{"result": []any{"c"}}})
	if !reflect.DeepEqual(wrapped, []string{"c"}) {
		t.Fatalf("wrapped = %#v", wrapped)
	}
}

func TestExtractPrometheusSeriesDefaultsSource(t *testing.T) {
	series := ExtractPrometheusSeries(map[string]any{"data": []any{map[string]any{"__name__": "x"}}})
	if len(series) != 1 || series[0][LabelSource] != SourceAWS {
		t.Fatalf("series = %#v, want source=aws default", series)
	}
}
