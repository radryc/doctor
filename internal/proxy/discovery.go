package proxy

import (
	"regexp"
	"sort"
	"strings"
)

// discoverySourceMatcher matches a positive `source` label matcher in a
// Prometheus selector. Negative matchers are left untouched: they do not tell
// us which backend owns the series.
var discoverySourceMatcher = regexp.MustCompile(`(?i)\bsource\s*(=|!=|=~|!~)\s*"([^"]*)"`)

// discoveryTargetMatcher matches labels that exist only for federation target
// selection. CloudWatch and Cloud Monitoring do not expose them as metric
// labels, so they must be stripped before forwarding a selector.
var discoveryTargetMatcher = regexp.MustCompile(`(?i)\b(?:account|aws_account|region|aws_region|partition|guardian_partition|gcp_project|project_id|gcp_region|location)\s*(?:=|!=|=~|!~)\s*"[^"]*"\s*,?\s*`)

// DiscoverySelectorsFor returns the selectors a backend can answer. It strips
// the Doctor-only `source` matcher (and, for AWS/GCP, target-selection labels)
// and skips selectors that explicitly name a different provider.
//
// ok is false when every selector names another provider, which lets the caller
// avoid querying that backend at all.
func DiscoverySelectorsFor(backend Backend, match []string) (forwarded []string, ok bool) {
	if len(match) == 0 {
		return nil, true
	}
	source := sourceForKind(backend.Kind)
	backendName := strings.ToLower(strings.TrimSpace(backend.Name))
	out := make([]string, 0, len(match))
	for _, selector := range match {
		include, stripped := discoverySelector(selector, source, backendName)
		if !include {
			continue
		}
		if backend.Kind == KindAWS || backend.Kind == KindGCP {
			stripped = stripDiscoveryTargetLabels(stripped)
		}
		out = append(out, stripped)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func discoverySelector(selector, backendSource, backendName string) (bool, string) {
	if !strings.Contains(strings.ToLower(selector), "source") {
		return true, selector
	}
	matches := discoverySourceMatcher.FindAllStringSubmatch(selector, -1)
	if len(matches) == 0 {
		return true, selector
	}
	include := true
	for _, match := range matches {
		op := match[1]
		if op == "!=" || op == "!~" {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(match[2]))
		if value == "" {
			continue
		}
		if !sourceValueMatches(op, value, backendSource, backendName) {
			include = false
			break
		}
	}
	if !include {
		return false, selector
	}
	return true, stripDiscoverySourceMatcher(selector)
}

func sourceValueMatches(op, value, backendSource, backendName string) bool {
	candidates := []string{strings.ToLower(backendSource), backendName}
	if op == "=~" {
		for _, part := range strings.Split(value, "|") {
			part = strings.TrimSpace(part)
			for _, candidate := range candidates {
				if candidate != "" && part == candidate {
					return true
				}
			}
		}
		return false
	}
	for _, candidate := range candidates {
		if candidate != "" && value == candidate {
			return true
		}
	}
	return false
}

func stripDiscoverySourceMatcher(selector string) string {
	out := discoverySourceMatcher.ReplaceAllStringFunc(selector, func(match string) string {
		sub := discoverySourceMatcher.FindStringSubmatch(match)
		if len(sub) == 3 && (sub[1] == "!=" || sub[1] == "!~") {
			return match
		}
		return ""
	})
	return cleanSelectorPunctuation(out)
}

func stripDiscoveryTargetLabels(selector string) string {
	return cleanSelectorPunctuation(discoveryTargetMatcher.ReplaceAllString(selector, ""))
}

var (
	discoveryLeadComma = regexp.MustCompile(`\{\s*,`)
	discoveryTailComma = regexp.MustCompile(`,\s*\}`)
)

func cleanSelectorPunctuation(selector string) string {
	out := discoveryLeadComma.ReplaceAllString(selector, "{")
	out = discoveryTailComma.ReplaceAllString(out, "}")
	out = strings.ReplaceAll(out, ",,", ",")
	out = strings.ReplaceAll(out, "{,", "{")
	out = strings.ReplaceAll(out, ",}", "}")
	out = strings.ReplaceAll(out, "{}", "")
	return strings.TrimSpace(out)
}

// StampSeriesSource fills in the provenance label for series returned by a
// backend. Existing source labels win.
func StampSeriesSource(series []map[string]string, source string) []map[string]string {
	if source == "" {
		return series
	}
	for _, labels := range series {
		if _, exists := labels[LabelSource]; !exists {
			labels[LabelSource] = source
		}
	}
	return series
}

// MergePrometheusSeries merges series label sets from several backends. Exact
// duplicates from the same backend collapse; identical label sets from
// different backends stay distinct and receive shard=<backend>.
func MergePrometheusSeries(groups []SeriesGroup) []map[string]string {
	type entry struct {
		labels  map[string]string
		backend string
		key     string
	}
	seen := make(map[string]struct{})
	entries := make([]entry, 0)
	for _, group := range groups {
		for _, labels := range group.Series {
			if labels == nil {
				continue
			}
			if _, exists := labels[LabelSource]; !exists && group.Source != "" {
				labels[LabelSource] = group.Source
			}
			key := seriesLabelKey(labels)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			entries = append(entries, entry{labels: labels, backend: group.backend(), key: key})
		}
	}
	// Collisions across backends are only possible when the source label is
	// identical. Split them so Grafana can tell the series apart.
	byKey := make(map[string][]*entry)
	order := make([]string, 0)
	for i := range entries {
		key := entries[i].key
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = append(byKey[key], &entries[i])
	}
	for _, key := range order {
		items := byKey[key]
		if len(items) < 2 {
			continue
		}
		backends := make(map[string]struct{}, len(items))
		for _, item := range items {
			backends[item.backend] = struct{}{}
		}
		if len(backends) < 2 {
			continue
		}
		counts := make(map[string]int, len(items))
		for _, item := range items {
			counts[item.backend]++
			shard := item.backend
			if counts[item.backend] > 1 {
				shard = item.backend + "#" + itoa(counts[item.backend])
			}
			item.labels[LabelShard] = shard
		}
	}
	out := make([]map[string]string, 0, len(entries))
	for _, item := range entries {
		out = append(out, item.labels)
	}
	sort.Slice(out, func(i, j int) bool {
		return seriesLabelKey(out[i]) < seriesLabelKey(out[j])
	})
	return out
}

// SeriesGroup is one backend's series-discovery result.
type SeriesGroup struct {
	Source  string
	Backend string
	Series  []map[string]string
}

func (g SeriesGroup) backend() string {
	if strings.TrimSpace(g.Backend) != "" {
		return g.Backend
	}
	return g.Source
}

// MergeStringLists unions string lists and returns a stable order.
func MergeStringLists(lists ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, list := range lists {
		for _, value := range list {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// LabelValue carries the provider that produced a discovery value. It lets the
// metric catalog keep a balanced Doctor/AWS/GCP mix when a limit is applied.
type LabelValue struct {
	Value  string
	Source string
}

// ValuesGroup is one backend's label-value discovery result.
type ValuesGroup struct {
	Source  string
	Backend string
	Values  []string
}

// MergeLabelValues unions label values and records the first provider that
// produced each value.
func MergeLabelValues(groups []ValuesGroup) []LabelValue {
	seen := make(map[string]struct{})
	out := make([]LabelValue, 0)
	for _, group := range groups {
		for _, value := range group.Values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, LabelValue{Value: value, Source: group.Source})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

func seriesLabelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(labels[key])
		builder.WriteByte(0)
	}
	return builder.String()
}
