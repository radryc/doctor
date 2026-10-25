// Package awsstore is a small HTTP client for the read-only CloudWatch/X-Ray
// API exposed by guardian-pusher-aws. Doctor federates these results into the
// same Grafana dataspace as its own OTLP telemetry.
package awsstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MetricSeries mirrors awsread.MetricSeries.
type MetricSeries struct {
	Namespace  string            `json:"namespace"`
	MetricName string            `json:"metric_name"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Region     string            `json:"region,omitempty"`
	Account    string            `json:"account,omitempty"`
	Unit       string            `json:"unit,omitempty"`
	Points     []MetricPoint     `json:"points"`
}

// MetricPoint mirrors awsread.MetricPoint.
type MetricPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// LogRecord mirrors awsread.LogRecord.
type LogRecord struct {
	Timestamp time.Time         `json:"timestamp"`
	LogGroup  string            `json:"log_group"`
	LogStream string            `json:"log_stream,omitempty"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	Region    string            `json:"region,omitempty"`
	Account   string            `json:"account,omitempty"`
}

// Span mirrors awsread.Span.
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
	Account      string            `json:"account,omitempty"`
	// Partition is filled by the federation manager from the catalog target.
	// The pusher read API does not know Guardian partitions.
	Partition string `json:"-"`
}

// TraceSummary mirrors awsread.TraceSummary.
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
	Account    string            `json:"account,omitempty"`
	// Partition is filled by the federation manager from the catalog target.
	Partition string `json:"-"`
}

// ServiceInfo mirrors awsread.ServiceInfo.
type ServiceInfo struct {
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	Account string `json:"account,omitempty"`
}

// MetricDescriptor mirrors awsread.MetricDescriptor.
type MetricDescriptor struct {
	Namespace  string   `json:"namespace"`
	MetricName string   `json:"metric_name"`
	Dimensions []string `json:"dimensions,omitempty"`
	Region     string   `json:"region,omitempty"`
}

// MetricQuery selects a CloudWatch metric series.
type MetricQuery struct {
	Namespace  string
	MetricName string
	Dimensions map[string]string
	Stat       string
	Period     time.Duration
	From       time.Time
	To         time.Time
}

// LogQuery selects CloudWatch log records.
type LogQuery struct {
	LogGroups []string
	Query     string
	From      time.Time
	To        time.Time
	Limit     int
}

// TraceQuery selects X-Ray traces.
type TraceQuery struct {
	Service string
	From    time.Time
	To      time.Time
	Limit   int
}

// TokenSource supplies and refreshes the bearer token for a pusher.
// Invalidate is called after a 401 so the next Token call bypasses its cache.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// HTTPError is returned for non-200 pusher responses. It keeps the status
// code available for retry/circuit-breaker decisions.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("aws backend HTTP %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
}

// Client talks to the pusher AWS read API.
type Client struct {
	baseURL     string
	token       string
	tokenSource TokenSource
	httpClient  *http.Client
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

// NewClientWithTokenSource builds a client that resolves its bearer token
// lazily and refreshes it once after a 401.
func NewClientWithTokenSource(baseURL string, source TokenSource) *Client {
	client := NewClient(baseURL, "")
	client.tokenSource = source
	return client
}

// Enabled reports whether a backend URL is configured.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// ListServices returns X-Ray services with their AWS partition metadata. It
// accepts both the object form (`[{"name":"api",...}]`) and the legacy string
// form (`["api"]`) so doctor and the pusher can be rolled independently.
func (c *Client) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/v1/aws/xray/services", nil, &raw); err != nil {
		return nil, err
	}
	var infos []ServiceInfo
	if err := json.Unmarshal(raw, &infos); err == nil {
		return infos, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("decode aws xray services: %w", err)
	}
	out := make([]ServiceInfo, 0, len(names))
	for _, name := range names {
		out = append(out, ServiceInfo{Name: name})
	}
	return out, nil
}

// ListMetrics discovers CloudWatch metrics, optionally filtered by namespace and
// metric name. CloudWatch's PromQL API does not implement the label-values
// route, so metric-name discovery for Grafana uses this REST endpoint instead.
func (c *Client) ListMetrics(ctx context.Context, namespace, metric string) ([]MetricDescriptor, error) {
	params := url.Values{}
	if namespace != "" {
		params.Set("namespace", namespace)
	}
	if metric != "" {
		params.Set("metric", metric)
	}
	var out []MetricDescriptor
	if err := c.get(ctx, "/v1/aws/metrics", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListLogGroups returns the CloudWatch log groups exposed by the pusher read
// API. It is used by the logs UI and Grafana variables to discover what can be
// queried. The pusher returns a bare JSON array; accept the legacy object form
// (`{"log_groups":[...]}`) as well so Doctor and the pusher can be rolled
// independently.
func (c *Client) ListLogGroups(ctx context.Context) ([]string, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/v1/aws/log-groups", nil, &raw); err != nil {
		return nil, err
	}
	var groups []string
	if err := json.Unmarshal(raw, &groups); err == nil {
		return groups, nil
	}
	var resp struct {
		LogGroups []string `json:"log_groups"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode aws log groups: %w", err)
	}
	return resp.LogGroups, nil
}

// QueryMetric fetches a CloudWatch metric series.
func (c *Client) QueryMetric(ctx context.Context, q MetricQuery) ([]MetricSeries, error) {
	params := url.Values{}
	params.Set("namespace", q.Namespace)
	params.Set("metric", q.MetricName)
	if q.Stat != "" {
		params.Set("stat", q.Stat)
	}
	if q.Period > 0 {
		params.Set("period", q.Period.String())
	}
	if !q.From.IsZero() {
		params.Set("from", q.From.UTC().Format(time.RFC3339Nano))
	}
	if !q.To.IsZero() {
		params.Set("to", q.To.UTC().Format(time.RFC3339Nano))
	}
	for name, value := range q.Dimensions {
		params.Set("dim."+name, value)
	}
	var out []MetricSeries
	if err := c.get(ctx, "/v1/aws/metrics/query", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryLogs runs a CloudWatch Logs Insights query.
func (c *Client) QueryLogs(ctx context.Context, q LogQuery) ([]LogRecord, error) {
	body := map[string]any{
		"log_groups": q.LogGroups,
		"query":      q.Query,
		"limit":      q.Limit,
	}
	if !q.From.IsZero() {
		body["from"] = q.From.UTC().Format(time.RFC3339Nano)
	}
	if !q.To.IsZero() {
		body["to"] = q.To.UTC().Format(time.RFC3339Nano)
	}
	var out []LogRecord
	if err := c.post(ctx, "/v1/aws/logs/query", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryTraces returns X-Ray trace summaries.
func (c *Client) QueryTraces(ctx context.Context, q TraceQuery) ([]TraceSummary, error) {
	params := url.Values{}
	if q.Service != "" {
		params.Set("service", q.Service)
	}
	if !q.From.IsZero() {
		params.Set("from", q.From.UTC().Format(time.RFC3339Nano))
	}
	if !q.To.IsZero() {
		params.Set("to", q.To.UTC().Format(time.RFC3339Nano))
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	var out []TraceSummary
	if err := c.get(ctx, "/v1/aws/xray/traces", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTrace returns the spans of an X-Ray trace.
func (c *Client) GetTrace(ctx context.Context, traceID string) ([]Span, error) {
	var out []Span
	if err := c.get(ctx, "/v1/aws/xray/trace/"+url.PathEscape(traceID), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PrometheusQuery proxies an instant PromQL query to CloudWatch.
func (c *Client) PrometheusQuery(ctx context.Context, query string, at time.Time) (map[string]any, error) {
	params := url.Values{}
	params.Set("query", query)
	if !at.IsZero() {
		params.Set("time", at.UTC().Format(time.RFC3339Nano))
	}
	return c.promql(ctx, "/v1/aws/promql/query", params)
}

// PrometheusQueryRange proxies a range PromQL query to CloudWatch.
func (c *Client) PrometheusQueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", start.UTC().Format(time.RFC3339Nano))
	params.Set("end", end.UTC().Format(time.RFC3339Nano))
	params.Set("step", step.String())
	return c.promql(ctx, "/v1/aws/promql/query_range", params)
}

// PrometheusSeries discovers CloudWatch series matching the selectors.
func (c *Client) PrometheusSeries(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	params := url.Values{}
	for _, selector := range match {
		params.Add("match[]", selector)
	}
	if !start.IsZero() {
		params.Set("start", start.UTC().Format(time.RFC3339Nano))
	}
	if !end.IsZero() {
		params.Set("end", end.UTC().Format(time.RFC3339Nano))
	}
	return c.promql(ctx, "/v1/aws/promql/series", params)
}

// PrometheusLabels discovers CloudWatch label names.
func (c *Client) PrometheusLabels(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	params := url.Values{}
	for _, selector := range match {
		params.Add("match[]", selector)
	}
	if !start.IsZero() {
		params.Set("start", start.UTC().Format(time.RFC3339Nano))
	}
	if !end.IsZero() {
		params.Set("end", end.UTC().Format(time.RFC3339Nano))
	}
	return c.promql(ctx, "/v1/aws/promql/labels", params)
}

// PrometheusLabelValues discovers values for a CloudWatch label.
func (c *Client) PrometheusLabelValues(ctx context.Context, label string, match []string, start, end time.Time) (map[string]any, error) {
	params := url.Values{}
	for _, selector := range match {
		params.Add("match[]", selector)
	}
	if !start.IsZero() {
		params.Set("start", start.UTC().Format(time.RFC3339Nano))
	}
	if !end.IsZero() {
		params.Set("end", end.UTC().Format(time.RFC3339Nano))
	}
	return c.promql(ctx, "/v1/aws/promql/label/"+url.PathEscape(label)+"/values", params)
}

func (c *Client) promql(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	body := params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	resp, err := c.doHTTP(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(payload)}
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("decode aws promql response: %w", err)
	}
	return decoded, nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	requestURL := c.baseURL + path
	if len(params) > 0 {
		requestURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(encoded)), nil
	}
	return c.do(req, out)
}

// doHTTP attaches authorization, retries once with a refreshed token after a
// 401, and returns the response without consuming its body.
func (c *Client) doHTTP(req *http.Request) (*http.Response, error) {
	req.Header.Set("Accept", "application/json")
	attempts := 1
	if c.tokenSource != nil {
		attempts = 2
	}
	for attempt := 0; attempt < attempts; attempt++ {
		current := req
		if attempt > 0 {
			current = req.Clone(req.Context())
			if req.Header != nil {
				current.Header = req.Header.Clone()
			}
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				current.Body = body
			}
		}
		token := c.token
		if c.tokenSource != nil {
			var err error
			token, err = c.tokenSource.Token(req.Context())
			if err != nil {
				return nil, err
			}
		}
		if token != "" {
			current.Header.Set("Authorization", "Bearer "+token)
		} else {
			current.Header.Del("Authorization")
		}
		resp, err := c.httpClient.Do(current)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && c.tokenSource != nil && attempt == 0 {
			_ = resp.Body.Close()
			c.tokenSource.Invalidate()
			continue
		}
		return resp, nil
	}
	return nil, &HTTPError{StatusCode: http.StatusUnauthorized}
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.doHTTP(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
