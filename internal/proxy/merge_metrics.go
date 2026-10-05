package proxy

import (
	"sort"
	"strings"
)

// PromRemote is one backend's Prometheus query result. Source is the
// provenance label stamped on every series (the backend kind, e.g.
// "prometheus" or "aws"); Backend is the backend name used for shard labeling
// and defaults to Source when empty.
type PromRemote struct {
	Source  string
	Backend string
	Data    map[string]any
}

// MergePrometheusQueryData merges remote Prometheus vector/matrix data into the
// local result, tagging remote series with source=<remote.Source>. When two
// backends return the same series label set, a shard=<backend> label is added
// to keep both series distinguishable.
func MergePrometheusQueryData(local map[string]any, defaultType string, remotes ...PromRemote) map[string]any {
	active := make([]PromRemote, 0, len(remotes))
	for _, remote := range remotes {
		if remote.Data != nil {
			active = append(active, remote)
		}
	}

	if local == nil && len(active) == 0 {
		return map[string]any{"resultType": defaultType, "result": []any{}}
	}

	if local == nil {
		first := active[0]
		merged := tagPrometheusData(first.Data, first.Source)
		refs := collectPrometheusRefs(PrometheusResultSlice(merged), first.backend())
		for _, remote := range active[1:] {
			var appended []promItemRef
			merged, appended = appendPrometheusData(merged, remote.Data, remote.Source, remote.backend())
			refs = append(refs, appended...)
		}
		labelPrometheusCollisions(refs)
		return merged
	}

	if len(active) == 0 {
		return local
	}

	localType, _ := local["resultType"].(string)
	localBackend := prometheusLocalBackend(local)
	localRefs := collectPrometheusRefs(PrometheusResultSlice(local), localBackend)
	var refs []promItemRef
	for _, remote := range active {
		remoteType, _ := remote.Data["resultType"].(string)
		if localType != "" && remoteType != "" && localType != remoteType {
			continue
		}
		var appended []promItemRef
		local, appended = appendPrometheusData(local, remote.Data, remote.Source, remote.backend())
		refs = append(refs, appended...)
	}
	if len(refs) == 0 {
		return local
	}
	refs = append(localRefs, refs...)
	labelPrometheusCollisions(refs)
	return local
}

func (r PromRemote) backend() string {
	if strings.TrimSpace(r.Backend) != "" {
		return r.Backend
	}
	return r.Source
}

// ExtractPrometheusData unwraps the "data" object of a Prometheus API response.
func ExtractPrometheusData(response map[string]any) map[string]any {
	if response == nil {
		return nil
	}
	data, _ := response["data"].(map[string]any)
	return data
}

// ExtractPrometheusStringList decodes label/label-value discovery responses.
// Prometheus returns these as a bare JSON array under "data"
// (`{"status":"success","data":["a","b"]}`), but some backends wrap the array
// in an object with a "result" field, so both shapes are accepted.
func ExtractPrometheusStringList(response map[string]any) []string {
	if response == nil {
		return nil
	}
	out := make([]string, 0)
	appendValue := func(value any) {
		if str, ok := value.(string); ok {
			out = append(out, str)
		}
	}
	switch data := response["data"].(type) {
	case []any:
		for _, value := range data {
			appendValue(value)
		}
	case map[string]any:
		if result, ok := data["result"].([]any); ok {
			for _, value := range result {
				appendValue(value)
			}
		}
	}
	return out
}

// ExtractPrometheusSeries decodes a series-discovery response, accepting both
// the Prometheus array-of-label-maps shape and a wrapped "result" object.
func ExtractPrometheusSeries(response map[string]any) []map[string]string {
	if response == nil {
		return nil
	}
	out := make([]map[string]string, 0)
	appendSeries := func(item any) {
		m, ok := item.(map[string]any)
		if !ok {
			return
		}
		labels := make(map[string]string, len(m)+1)
		for key, raw := range m {
			if str, ok := raw.(string); ok {
				labels[key] = str
			}
		}
		if _, exists := labels[LabelSource]; !exists {
			labels[LabelSource] = SourceAWS
		}
		out = append(out, labels)
	}
	switch data := response["data"].(type) {
	case []any:
		for _, item := range data {
			appendSeries(item)
		}
	case map[string]any:
		if result, ok := data["result"].([]any); ok {
			for _, item := range result {
				appendSeries(item)
			}
		}
	}
	return out
}

// PrometheusResultSlice returns data["result"] as a []any regardless of whether
// it was built in-process (concrete slices) or decoded from JSON ([]any).
func PrometheusResultSlice(data map[string]any) []any {
	if data == nil {
		return nil
	}
	switch result := data["result"].(type) {
	case []any:
		return result
	case []map[string]any:
		out := make([]any, 0, len(result))
		for _, item := range result {
			out = append(out, item)
		}
		return out
	default:
		return nil
	}
}

// PrometheusResultEmpty reports whether a Prometheus query result contains no
// series. A nil result is treated as empty.
func PrometheusResultEmpty(data map[string]any) bool {
	return len(PrometheusResultSlice(data)) == 0
}

func appendPrometheusData(local, remote map[string]any, source, backend string) (map[string]any, []promItemRef) {
	if local == nil {
		local = map[string]any{}
	}
	localResult := PrometheusResultSlice(local)
	refs := make([]promItemRef, 0)
	for _, item := range PrometheusResultSlice(remote) {
		item = tagPrometheusItem(item, source)
		if labels := prometheusItemLabels(item); labels != nil {
			refs = append(refs, promItemRef{labels: labels, backend: backend})
		}
		localResult = append(localResult, item)
	}
	local["result"] = localResult
	return local, refs
}

func tagPrometheusData(data map[string]any, source string) map[string]any {
	if data == nil {
		return nil
	}
	if result := PrometheusResultSlice(data); result != nil {
		tagged := make([]any, 0, len(result))
		for _, item := range result {
			tagged = append(tagged, tagPrometheusItem(item, source))
		}
		data["result"] = tagged
	}
	return data
}

func tagPrometheusItem(item any, source string) any {
	m, ok := item.(map[string]any)
	if !ok {
		return item
	}
	if metric, ok := m["metric"].(map[string]any); ok {
		if _, exists := metric[LabelSource]; !exists {
			metric[LabelSource] = source
		}
		m["metric"] = metric
		return m
	}
	// series-style item: the map itself is the label set
	if _, exists := m[LabelSource]; !exists {
		m[LabelSource] = source
	}
	return m
}

// promItemRef ties a mutable label set to the backend that produced it so
// cross-backend collisions can be sharded without relying on labels.
type promItemRef struct {
	labels  map[string]any
	backend string
}

func collectPrometheusRefs(items []any, backend string) []promItemRef {
	refs := make([]promItemRef, 0, len(items))
	for _, item := range items {
		if labels := prometheusItemLabels(item); labels != nil {
			refs = append(refs, promItemRef{labels: labels, backend: backend})
		}
	}
	return refs
}

// labelPrometheusCollisions adds shard labels to series that share an identical
// label set across backends. Duplicates from a single backend keep their
// original shape.
func labelPrometheusCollisions(refs []promItemRef) {
	if len(refs) < 2 {
		return
	}
	groups := make(map[string][]promItemRef)
	order := make([]string, 0, len(refs))
	for _, ref := range refs {
		if _, exists := ref.labels[LabelShard]; exists {
			continue
		}
		key := prometheusLabelKey(ref.labels)
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], ref)
	}

	for _, key := range order {
		group := groups[key]
		if len(group) < 2 {
			continue
		}
		distinct := make(map[string]struct{}, len(group))
		for _, ref := range group {
			distinct[ref.backend] = struct{}{}
		}
		if len(distinct) < 2 {
			continue
		}
		perBackend := make(map[string]int)
		for _, ref := range group {
			perBackend[ref.backend]++
			shard := ref.backend
			if perBackend[ref.backend] > 1 {
				shard = ref.backend + "#" + itoa(perBackend[ref.backend])
			}
			ref.labels[LabelShard] = shard
		}
	}
}

func prometheusLocalBackend(data map[string]any) string {
	for _, item := range PrometheusResultSlice(data) {
		if labels := prometheusItemLabels(item); labels != nil {
			if source, ok := labels[LabelSource].(string); ok && source != "" {
				return source
			}
		}
	}
	return SourceDoctor
}

// prometheusItemLabels returns the mutable label map of a Prometheus result
// item (vector/matrix sample or series label set).
func prometheusItemLabels(item any) map[string]any {
	m, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	if metric, ok := m["metric"].(map[string]any); ok {
		return metric
	}
	if _, hasValue := m["value"]; hasValue {
		return nil
	}
	if _, hasValues := m["values"]; hasValues {
		return nil
	}
	return m
}

func prometheusLabelKey(labels map[string]any) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		if value, ok := labels[key].(string); ok {
			builder.WriteString(value)
		}
		builder.WriteByte(0)
	}
	return builder.String()
}

func itoa(value int) string {
	if value < 10 {
		return string(rune('0' + value))
	}
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}
