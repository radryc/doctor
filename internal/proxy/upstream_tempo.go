package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/doctor/internal/tempo"
)

// TempoHTTPUpstream queries a remote Tempo-compatible HTTP API.
type TempoHTTPUpstream struct {
	name    string
	baseURL string
	token   string
	tenant  string
	client  *http.Client
}

// NewTempoHTTPUpstream builds a Tempo HTTP adapter for a backend.
func NewTempoHTTPUpstream(backend Backend, client *http.Client) *TempoHTTPUpstream {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &TempoHTTPUpstream{
		name:    backend.Name,
		baseURL: strings.TrimRight(backend.BaseURL, "/"),
		token:   backend.Token,
		tenant:  backend.Tenant,
		client:  client,
	}
}

// Name implements TracesUpstream.
func (u *TempoHTTPUpstream) Name() string { return u.name }

// ListServices implements TracesUpstream.
func (u *TempoHTTPUpstream) ListServices(ctx context.Context, tenant string) ([]string, error) {
	return u.TraceTagValues(ctx, tenant, "service.name")
}

// SearchTraces implements TracesUpstream.
func (u *TempoHTTPUpstream) SearchTraces(ctx context.Context, tenant string, search TraceSearch) ([]TraceSummary, error) {
	query := url.Values{}
	if search.Service != "" {
		query.Add("tags", "service.name="+search.Service)
	}
	for _, tag := range search.Tags {
		query.Add("tags", tag.Key+"="+tag.Value)
	}
	setTempoRange(query, search.From, search.To)
	if search.Limit > 0 {
		query.Set("limit", strconv.Itoa(search.Limit))
	}
	if search.MinDuration > 0 {
		query.Set("minDuration", search.MinDuration.String())
	}
	if search.MaxDuration > 0 {
		query.Set("maxDuration", search.MaxDuration.String())
	}
	var response tempo.SearchResponse
	if err := u.getJSON(ctx, tenant, "/api/search", query, &response); err != nil {
		return nil, err
	}
	return summariesFromTempo(response.Traces), nil
}

// SearchTraceQL implements TraceQLUpstream.
func (u *TempoHTTPUpstream) SearchTraceQL(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]TraceSummary, error) {
	values := url.Values{}
	values.Set("q", query)
	setTempoRange(values, from, to)
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	var response tempo.SearchResponse
	if err := u.getJSON(ctx, tenant, "/api/search", values, &response); err != nil {
		return nil, err
	}
	return summariesFromTempo(response.Traces), nil
}

// GetTrace implements TracesUpstream.
func (u *TempoHTTPUpstream) GetTrace(ctx context.Context, tenant, traceID string) ([]segment.TraceRecord, bool, error) {
	var trace tempo.Trace
	if err := u.getJSON(ctx, tenant, "/api/traces/"+url.PathEscape(traceID), nil, &trace); err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	records := recordsFromTempo(trace)
	if len(records) == 0 {
		return nil, false, nil
	}
	return records, true, nil
}

// TraceTags implements TraceTagSource.
func (u *TempoHTTPUpstream) TraceTags(ctx context.Context, tenant string) ([]string, error) {
	var response tempo.TagNamesResponse
	if err := u.getJSON(ctx, tenant, "/api/search/tags", nil, &response); err != nil {
		return nil, err
	}
	return response.TagNames, nil
}

// TraceTagValues implements TraceTagSource.
func (u *TempoHTTPUpstream) TraceTagValues(ctx context.Context, tenant, tag string) ([]string, error) {
	var response tempo.TagValuesResponse
	if err := u.getJSON(ctx, tenant, "/api/search/tag/"+url.PathEscape(tag)+"/values", nil, &response); err != nil {
		return nil, err
	}
	values := make([]string, 0, len(response.TagValues))
	for _, value := range response.TagValues {
		if value.Value != "" {
			values = append(values, value.Value)
		}
	}
	return values, nil
}

func setTempoRange(query url.Values, from, to time.Time) {
	if !from.IsZero() {
		query.Set("start", strconv.FormatInt(from.Unix(), 10))
	}
	if !to.IsZero() {
		query.Set("end", strconv.FormatInt(to.Unix(), 10))
	}
}

func (u *TempoHTTPUpstream) getJSON(ctx context.Context, tenant, path string, query url.Values, out any) error {
	target := u.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	if orgID := firstNonEmpty(tenant, u.tenant); orgID != "" && orgID != "default" {
		req.Header.Set("X-Scope-OrgID", orgID)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("tempo %s: %w", u.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return &notFoundError{backend: u.name, path: path}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("tempo %s: HTTP %d: %s", u.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return fmt.Errorf("tempo %s: decode: %w", u.name, err)
	}
	return nil
}

type notFoundError struct {
	backend string
	path    string
}

func (e *notFoundError) Error() string {
	return fmt.Sprintf("tempo %s: %s not found", e.backend, e.path)
}

func isNotFound(err error) bool {
	var notFound *notFoundError
	return errors.As(err, &notFound)
}

func summariesFromTempo(traces []tempo.SearchTrace) []TraceSummary {
	out := make([]TraceSummary, 0, len(traces))
	for _, trace := range traces {
		start := time.Time{}
		if nanos, err := strconv.ParseInt(trace.StartTimeUnixNano, 10, 64); err == nil {
			start = time.Unix(0, nanos).UTC()
		}
		out = append(out, TraceSummary{
			TraceID:         trace.TraceID,
			RootServiceName: trace.RootServiceName,
			RootTraceName:   trace.RootTraceName,
			StartTime:       start,
			Duration:        time.Duration(trace.DurationMs) * time.Millisecond,
		})
	}
	return out
}

func recordsFromTempo(trace tempo.Trace) []segment.TraceRecord {
	records := make([]segment.TraceRecord, 0)
	for _, batch := range trace.Batches {
		resourceAttributes := keyValuesToMap(batch.Resource.Attributes)
		service := resourceAttributes["service.name"]
		for _, scopeSpans := range batch.ScopeSpans {
			for _, span := range scopeSpans.Spans {
				record := segment.TraceRecord{
					TraceID:            span.TraceID,
					SpanID:             span.SpanID,
					ParentSpanID:       span.ParentSpanID,
					Name:               span.Name,
					Kind:               span.Kind,
					Service:            service,
					ResourceAttributes: resourceAttributes,
					Attributes:         keyValuesToMap(span.Attributes),
				}
				if nanos, err := strconv.ParseInt(span.StartTimeUnixNano, 10, 64); err == nil {
					record.StartTime = time.Unix(0, nanos).UTC()
				}
				if nanos, err := strconv.ParseInt(span.EndTimeUnixNano, 10, 64); err == nil {
					record.EndTime = time.Unix(0, nanos).UTC()
				}
				if span.Status != nil {
					record.StatusCode = span.Status.Code
					record.StatusMessage = span.Status.Message
				}
				for _, event := range span.Events {
					traceEvent := segment.TraceEvent{
						Name:       event.Name,
						Attributes: keyValuesToMap(event.Attributes),
					}
					if nanos, err := strconv.ParseInt(event.TimeUnixNano, 10, 64); err == nil {
						traceEvent.Timestamp = time.Unix(0, nanos).UTC()
					}
					record.Events = append(record.Events, traceEvent)
				}
				records = append(records, record)
			}
		}
	}
	return records
}

func keyValuesToMap(attributes []tempo.KeyValue) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	out := make(map[string]string, len(attributes))
	for _, attribute := range attributes {
		out[attribute.Key] = attribute.Value.StringValue
	}
	return out
}
