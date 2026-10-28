package query

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

// This file implements the Loki index endpoints Grafana's Logs Drilldown and
// Explore use for volume histograms, index stats, patterns, and feature
// detection. The volume endpoints count stored log records per step; pattern
// mining and field detection are not implemented and return valid empty
// payloads so the drilldown degrades gracefully.

// lokiVolumeRecordLimit bounds the records scanned for volume/stats queries.
// It matches the existing metric-query ceiling (20k records) so the volume
// endpoints cannot hold more memory than a LogQL metric query.
const lokiVolumeRecordLimit = 20000

// Volume output caps: series (label combinations) and buckets (step points).
// Loki's own volume API accepts a series limit; 200 keeps a high-cardinality
// targetLabels selection from exhausting memory.
const (
	lokiVolumeMaxSeries  = 200
	lokiVolumeMaxBuckets = 5000
)

// lokiBuildVersion is reported by /loki/api/v1/status/buildinfo. Grafana uses
// it for feature detection; 3.x advertises volume, patterns, and detected
// fields support.
const lokiBuildVersion = "3.5.0"

// handleLokiBuildInfo serves GET /loki/api/v1/status/buildinfo.
func (s *Service) handleLokiBuildInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version":   lokiBuildVersion,
		"revision":  "doctor",
		"branch":    "",
		"buildDate": "",
		"buildUser": "",
		"goVersion": "",
	})
}

// handleLokiIndexStats serves GET /loki/api/v1/index/stats.
func (s *Service) handleLokiIndexStats(w http.ResponseWriter, r *http.Request) {
	start, end, plan, err := s.lokiVolumeRequest(r)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}
	records, err := s.fetchLokiRecords(r.Context(), s.tenantFromRequest(r), plan, start, end, lokiVolumeRecordLimit)
	if err != nil {
		writeLokiQueryError(w, err)
		return
	}
	records = plan.applyStages(records)

	streams := make(map[string]struct{})
	bytes := 0
	for _, record := range records {
		streams[metricLabelMapString(lokiLabelsFromRecord(record))] = struct{}{}
		bytes += len(record.Body)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"streams": len(streams),
		"chunks":  0,
		"entries": len(records),
		"bytes":   bytes,
	})
}

// handleLokiIndexVolume serves GET /loki/api/v1/index/volume (instant vector).
func (s *Service) handleLokiIndexVolume(w http.ResponseWriter, r *http.Request) {
	start, end, plan, err := s.lokiVolumeRequest(r)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}
	records, err := s.fetchLokiRecords(r.Context(), s.tenantFromRequest(r), plan, start, end, lokiVolumeRecordLimit)
	if err != nil {
		writeLokiQueryError(w, err)
		return
	}
	records = plan.applyStages(records)

	targetLabels := lokiTargetLabels(r)
	groups, order := groupLogRecordsByLabels(records, targetLabels)
	result := make([]map[string]any, 0, len(order))
	for _, key := range order {
		result = append(result, map[string]any{
			"metric": groups[key].labels,
			"value":  []any{float64(end.UnixMilli()) / 1000, strconv.Itoa(groups[key].count)},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result":     result,
		},
	})
}

// handleLokiIndexVolumeRange serves GET /loki/api/v1/index/volume_range
// (matrix): the log volume histogram Grafana Explore shows above log results.
func (s *Service) handleLokiIndexVolumeRange(w http.ResponseWriter, r *http.Request) {
	start, end, plan, err := s.lokiVolumeRequest(r)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}
	records, err := s.fetchLokiRecords(r.Context(), s.tenantFromRequest(r), plan, start, end, lokiVolumeRecordLimit)
	if err != nil {
		writeLokiQueryError(w, err)
		return
	}
	records = plan.applyStages(records)

	step := parseLokiStep(r.URL.Query().Get("step"), start, end)
	if step <= 0 {
		step = time.Minute
	}
	// Clamp the step so the response cannot grow beyond lokiVolumeMaxBuckets.
	if span := end.Sub(start); span > 0 {
		if minStep := span / lokiVolumeMaxBuckets; step < minStep {
			step = minStep
		}
	}
	targetLabels := lokiTargetLabels(r)

	type bucketSeries struct {
		labels map[string]string
		values map[int]int
	}
	groups := make(map[string]*bucketSeries)
	order := make([]string, 0)
	for _, record := range records {
		labels := selectLokiLabels(lokiLabelsFromRecord(record), targetLabels)
		key := metricLabelMapString(labels)
		series := groups[key]
		if series == nil {
			// Bound the series count for high-cardinality groupings; the
			// remaining records are folded into nothing, matching Loki's
			// series-limit behaviour.
			if len(groups) >= lokiVolumeMaxSeries {
				continue
			}
			series = &bucketSeries{labels: labels, values: make(map[int]int)}
			groups[key] = series
			order = append(order, key)
		}
		bucket := int(record.Timestamp.Sub(start) / step)
		if bucket < 0 {
			bucket = 0
		}
		if bucket >= lokiVolumeMaxBuckets {
			continue
		}
		series.values[bucket]++
	}
	sort.Strings(order)

	result := make([]map[string]any, 0, len(order))
	for _, key := range order {
		series := groups[key]
		values := make([][]any, 0, len(series.values))
		for bucket := 0; ; bucket++ {
			ts := start.Add(time.Duration(bucket) * step)
			if ts.After(end) {
				break
			}
			values = append(values, []any{float64(ts.UnixMilli()) / 1000, strconv.Itoa(series.values[bucket])})
		}
		result = append(result, map[string]any{
			"metric": series.labels,
			"values": values,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "matrix",
			"result":     result,
		},
	})
}

// handleLokiPatterns serves GET /loki/api/v1/patterns. Pattern mining is not
// implemented; an empty pattern list keeps the Logs Drilldown usable.
func (s *Service) handleLokiPatterns(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"data":   []any{},
	})
}

// handleLokiDetectedFields serves GET /loki/api/v1/detected_fields.
func (s *Service) handleLokiDetectedFields(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"detectedFields": []any{},
		"limit":          0,
		"query":          strings.TrimSpace(r.URL.Query().Get("query")),
	})
}

// handleLokiDetectedLabels serves GET /loki/api/v1/detected_labels.
func (s *Service) handleLokiDetectedLabels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"detectedLabels": []any{},
	})
}

// lokiVolumeRequest parses the shared volume/stats parameters.
func (s *Service) lokiVolumeRequest(r *http.Request) (time.Time, time.Time, lokiPlan, error) {
	now := time.Now().UTC()
	end, err := parseLokiTime(r.URL.Query().Get("end"), now)
	if err != nil {
		return time.Time{}, time.Time{}, lokiPlan{}, err
	}
	start, err := parseLokiTime(r.URL.Query().Get("start"), end.Add(-time.Hour))
	if err != nil {
		return time.Time{}, time.Time{}, lokiPlan{}, err
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, lokiPlan{}, fmt.Errorf("end must be after start")
	}
	plan, err := parseLokiPlan(strings.TrimSpace(r.URL.Query().Get("query")))
	if err != nil {
		return time.Time{}, time.Time{}, lokiPlan{}, err
	}
	return start, end, plan, nil
}

// lokiTargetLabels reads the optional targetLabels grouping parameter.
func lokiTargetLabels(r *http.Request) []string {
	raw := strings.TrimSpace(r.URL.Query().Get("targetLabels"))
	if raw == "" {
		return nil
	}
	labels := make([]string, 0, 2)
	for _, label := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(label); trimmed != "" {
			labels = append(labels, trimmed)
		}
	}
	return labels
}

func selectLokiLabels(labels map[string]string, target []string) map[string]string {
	if len(target) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(target))
	for _, name := range target {
		out[name] = labels[name]
	}
	return out
}

type lokiVolumeGroup struct {
	labels map[string]string
	count  int
}

func groupLogRecordsByLabels(records []segment.LogRecord, target []string) (map[string]*lokiVolumeGroup, []string) {
	groups := make(map[string]*lokiVolumeGroup)
	order := make([]string, 0)
	for _, record := range records {
		labels := selectLokiLabels(lokiLabelsFromRecord(record), target)
		key := metricLabelMapString(labels)
		group := groups[key]
		if group == nil {
			group = &lokiVolumeGroup{labels: labels}
			groups[key] = group
			order = append(order, key)
		}
		group.count++
	}
	sort.Strings(order)
	return groups, order
}
