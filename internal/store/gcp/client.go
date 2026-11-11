// Package gcpstore is a small HTTP client for the read-only GCP metrics, logs,
// and traces API exposed by guardian-pusher-gcp. Doctor federates these results
// into the same Grafana dataspace as its own OTLP telemetry.
package gcpstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MetricDescriptor mirrors gcpread.MetricDescriptor.
type MetricDescriptor struct {
	Namespace  string   `json:"namespace"`
	MetricName string   `json:"metric_name"`
	Dimensions []string `json:"dimensions,omitempty"`
	Region     string   `json:"region,omitempty"`
	Project    string   `json:"project,omitempty"`
}

// MetricPoint mirrors gcpread.MetricPoint.
type MetricPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// MetricSeries mirrors gcpread.MetricSeries.
type MetricSeries struct {
	Namespace  string            `json:"namespace"`
	MetricName string            `json:"metric_name"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Region     string            `json:"region,omitempty"`
	Project    string            `json:"project,omitempty"`
	Unit       string            `json:"unit,omitempty"`
	Points     []MetricPoint     `json:"points"`
}

// LogRecord mirrors gcpread.LogRecord.
type LogRecord struct {
	Timestamp time.Time         `json:"timestamp"`
	LogGroup  string            `json:"log_group"`
	LogStream string            `json:"log_stream,omitempty"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	Region    string            `json:"region,omitempty"`
	Project   string            `json:"project,omitempty"`
	Severity  string            `json:"severity,omitempty"`
}

// Span mirrors gcpread.Span.
type Span struct {
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	Name         string            `json:"name"`
	Service      string            `json:"service"`
	StartTime    time.Time         `json:"start_time"`
	EndTime      time.Time         `json:"end_time"`
	StatusCode   string            `json:"status_code,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	Region       string            `json:"region,omitempty"`
	Project      string            `json:"project,omitempty"`
	Partition    string            `json:"-"`
}

// TraceSummary mirrors gcpread.TraceSummary.
type TraceSummary struct {
	TraceID    string            `json:"trace_id"`
	Service    string            `json:"service,omitempty"`
	StartTime  time.Time         `json:"start_time"`
	EndTime    time.Time         `json:"end_time"`
	Duration   time.Duration     `json:"duration"`
	Status     string            `json:"status,omitempty"`
	SpanCount  int               `json:"span_count,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Region     string            `json:"region,omitempty"`
	Project    string            `json:"project,omitempty"`
	Partition  string            `json:"-"`
}

// ServiceInfo mirrors gcpread.ServiceInfo.
type ServiceInfo struct {
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	Project string `json:"project,omitempty"`
}

// MetricQuery selects a Cloud Monitoring series.
type MetricQuery struct {
	Namespace    string
	MetricName   string
	Dimensions   map[string]string
	ResourceType string
	Aligner      string
	From         time.Time
	To           time.Time
}

// LogQuery selects Cloud Logging entries.
type LogQuery struct {
	LogName string
	Query   string
	From    time.Time
	To      time.Time
	Limit   int
}

// TraceQuery selects Cloud Trace traces.
type TraceQuery struct {
	Service string
	From    time.Time
	To      time.Time
	Limit   int
}

// TokenSource supplies and refreshes the bearer token for a pusher.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// HTTPError is returned for non-2xx pusher responses.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("gcp backend HTTP %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
}

// Client talks to the pusher GCP read API.
type Client struct {
	baseURL     string
	token       string
	tokenSource TokenSource
	httpClient  *http.Client

	healthMu      sync.Mutex
	healthChecked time.Time
	healthErr     error
}

// NewClient builds a client for the given base URL.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

// NewClientWithTokenSource resolves its bearer token lazily.
func NewClientWithTokenSource(baseURL string, source TokenSource) *Client {
	client := NewClient(baseURL, "")
	client.tokenSource = source
	return client
}

// Enabled reports whether a backend URL is configured.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

const (
	healthCacheTTL = 10 * time.Second
	healthTimeout  = 2 * time.Second
)

// Health probes /healthz and caches the result briefly.
func (c *Client) Health(ctx context.Context) error {
	if !c.Enabled() {
		return fmt.Errorf("gcp backend is not configured")
	}
	c.healthMu.Lock()
	if !c.healthChecked.IsZero() && time.Since(c.healthChecked) < healthCacheTTL {
		err := c.healthErr
		c.healthMu.Unlock()
		return err
	}
	c.healthMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, c.baseURL+"/healthz", nil)
	var status int
	if err == nil {
		resp, doErr := c.doHTTP(req)
		if doErr == nil {
			status = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			if status < 200 || status >= 300 {
				err = &HTTPError{StatusCode: status, Body: "healthz"}
			}
		} else {
			err = doErr
		}
	}

	c.healthMu.Lock()
	c.healthChecked = time.Now()
	c.healthErr = err
	c.healthMu.Unlock()
	return err
}

func (c *Client) doHTTP(req *http.Request) (*http.Response, error) {
	if c.tokenSource != nil {
		token, err := c.tokenSource.Token(req.Context())
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	} else if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	return c.httpClient.Do(req)
}

func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out any) error {
	endpoint := c.baseURL + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.doHTTP(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(payload)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// ListMetrics returns Cloud Monitoring metric descriptors.
func (c *Client) ListMetrics(ctx context.Context, namespace, metric string) ([]MetricDescriptor, error) {
	params := url.Values{}
	if namespace != "" {
		params.Set("namespace", namespace)
	}
	if metric != "" {
		params.Set("metric", metric)
	}
	var response struct {
		Metrics []MetricDescriptor `json:"metrics"`
	}
	if err := c.getJSON(ctx, "/v1/gcp/metrics", params, &response); err != nil {
		return nil, err
	}
	return response.Metrics, nil
}

// QueryMetric returns Cloud Monitoring time series.
func (c *Client) QueryMetric(ctx context.Context, q MetricQuery) ([]MetricSeries, error) {
	params := url.Values{}
	params.Set("namespace", q.Namespace)
	params.Set("metric", q.MetricName)
	if q.ResourceType != "" {
		params.Set("resource_type", q.ResourceType)
	}
	if q.Aligner != "" {
		params.Set("aligner", q.Aligner)
	}
	if !q.From.IsZero() {
		params.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		params.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	for key, value := range q.Dimensions {
		params.Set("dim."+key, value)
	}
	var response struct {
		Series []MetricSeries `json:"series"`
	}
	if err := c.getJSON(ctx, "/v1/gcp/metrics/query", params, &response); err != nil {
		return nil, err
	}
	return response.Series, nil
}

// ListServices returns trace-derived service names.
func (c *Client) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	var response struct {
		Services []ServiceInfo `json:"services"`
	}
	if err := c.getJSON(ctx, "/v1/gcp/services", nil, &response); err != nil {
		return nil, err
	}
	return response.Services, nil
}

// ListLogNames returns discovered Cloud Logging log names.
func (c *Client) ListLogNames(ctx context.Context) ([]string, error) {
	var response struct {
		LogNames []string `json:"logNames"`
	}
	if err := c.getJSON(ctx, "/v1/gcp/log-names", nil, &response); err != nil {
		return nil, err
	}
	return response.LogNames, nil
}

// QueryLogs returns Cloud Logging entries.
func (c *Client) QueryLogs(ctx context.Context, q LogQuery) ([]LogRecord, error) {
	params := url.Values{}
	if q.LogName != "" {
		params.Set("log", q.LogName)
	}
	if q.Query != "" {
		params.Set("query", q.Query)
	}
	if !q.From.IsZero() {
		params.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		params.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	var response struct {
		Records []LogRecord `json:"records"`
	}
	if err := c.getJSON(ctx, "/v1/gcp/logs/query", params, &response); err != nil {
		return nil, err
	}
	return response.Records, nil
}

// QueryTraces returns Cloud Trace summaries.
func (c *Client) QueryTraces(ctx context.Context, q TraceQuery) ([]TraceSummary, error) {
	params := url.Values{}
	if q.Service != "" {
		params.Set("service", q.Service)
	}
	if !q.From.IsZero() {
		params.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		params.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	var response struct {
		Traces []TraceSummary `json:"traces"`
	}
	if err := c.getJSON(ctx, "/v1/gcp/traces", params, &response); err != nil {
		return nil, err
	}
	return response.Traces, nil
}

// GetTrace returns all spans of a trace.
func (c *Client) GetTrace(ctx context.Context, traceID string) ([]Span, error) {
	var response struct {
		Spans []Span `json:"spans"`
	}
	if err := c.getJSON(ctx, "/v1/gcp/traces/"+url.PathEscape(traceID), nil, &response); err != nil {
		return nil, err
	}
	return response.Spans, nil
}

// --------------------------------------------------------------------------
// PromQL
// --------------------------------------------------------------------------

func (c *Client) promQL(ctx context.Context, suffix string, params url.Values) (map[string]any, error) {
	endpoint := c.baseURL + "/v1/gcp/promql" + suffix
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.doHTTP(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(payload)}
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PrometheusQuery forwards an instant PromQL query.
func (c *Client) PrometheusQuery(ctx context.Context, query string, at time.Time) (map[string]any, error) {
	params := url.Values{}
	params.Set("query", query)
	if !at.IsZero() {
		params.Set("time", strconv.FormatFloat(float64(at.Unix()), 'f', 0, 64))
	}
	return c.promQL(ctx, "/api/v1/query", params)
}

// PrometheusQueryRange forwards a range PromQL query.
func (c *Client) PrometheusQueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatFloat(float64(start.Unix()), 'f', 0, 64))
	params.Set("end", strconv.FormatFloat(float64(end.Unix()), 'f', 0, 64))
	params.Set("step", strconv.FormatFloat(step.Seconds(), 'f', 0, 64))
	return c.promQL(ctx, "/api/v1/query_range", params)
}

// PrometheusSeries forwards a series lookup.
func (c *Client) PrometheusSeries(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	params := url.Values{}
	for _, selector := range match {
		params.Add("match[]", selector)
	}
	if !start.IsZero() {
		params.Set("start", strconv.FormatFloat(float64(start.Unix()), 'f', 0, 64))
	}
	if !end.IsZero() {
		params.Set("end", strconv.FormatFloat(float64(end.Unix()), 'f', 0, 64))
	}
	return c.promQL(ctx, "/api/v1/series", params)
}

// PrometheusLabels forwards a label-name lookup.
func (c *Client) PrometheusLabels(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	params := url.Values{}
	for _, selector := range match {
		params.Add("match[]", selector)
	}
	if !start.IsZero() {
		params.Set("start", strconv.FormatFloat(float64(start.Unix()), 'f', 0, 64))
	}
	if !end.IsZero() {
		params.Set("end", strconv.FormatFloat(float64(end.Unix()), 'f', 0, 64))
	}
	return c.promQL(ctx, "/api/v1/labels", params)
}

// PrometheusLabelValues forwards a label-value lookup.
func (c *Client) PrometheusLabelValues(ctx context.Context, label string, match []string, start, end time.Time) (map[string]any, error) {
	params := url.Values{}
	for _, selector := range match {
		params.Add("match[]", selector)
	}
	if !start.IsZero() {
		params.Set("start", strconv.FormatFloat(float64(start.Unix()), 'f', 0, 64))
	}
	if !end.IsZero() {
		params.Set("end", strconv.FormatFloat(float64(end.Unix()), 'f', 0, 64))
	}
	return c.promQL(ctx, "/api/v1/label/"+url.PathEscape(label)+"/values", params)
}
