package proxy

import (
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

// PromHTTPUpstream queries a remote Prometheus-compatible HTTP API.
type PromHTTPUpstream struct {
	name    string
	baseURL string
	token   string
	tenant  string
	client  *http.Client
}

// NewPromHTTPUpstream builds a Prometheus HTTP adapter for a backend.
func NewPromHTTPUpstream(backend Backend, client *http.Client) *PromHTTPUpstream {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &PromHTTPUpstream{
		name:    backend.Name,
		baseURL: strings.TrimRight(backend.BaseURL, "/"),
		token:   backend.Token,
		tenant:  backend.Tenant,
		client:  client,
	}
}

// Name implements MetricsUpstream.
func (u *PromHTTPUpstream) Name() string { return u.name }

// PromInstant implements MetricsUpstream.
func (u *PromHTTPUpstream) PromInstant(ctx context.Context, _, query string, at time.Time) (map[string]any, error) {
	form := url.Values{}
	form.Set("query", query)
	form.Set("time", strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64))
	var response map[string]any
	if err := u.do(ctx, "/api/v1/query", form, &response); err != nil {
		return nil, err
	}
	return ExtractPrometheusData(response), nil
}

// PromRange implements MetricsUpstream.
func (u *PromHTTPUpstream) PromRange(ctx context.Context, _, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	form := url.Values{}
	form.Set("query", query)
	form.Set("start", strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64))
	form.Set("end", strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64))
	form.Set("step", strconv.FormatFloat(step.Seconds(), 'f', 3, 64))
	var response map[string]any
	if err := u.do(ctx, "/api/v1/query_range", form, &response); err != nil {
		return nil, err
	}
	return ExtractPrometheusData(response), nil
}

// PromSeries implements MetricsUpstream.
func (u *PromHTTPUpstream) PromSeries(ctx context.Context, _ string, match []string, start, end time.Time) ([]map[string]string, error) {
	form := url.Values{}
	for _, selector := range match {
		form.Add("match[]", selector)
	}
	setTimeRange(form, start, end)
	var response map[string]any
	if err := u.do(ctx, "/api/v1/series", form, &response); err != nil {
		return nil, err
	}
	return ExtractPrometheusSeries(response), nil
}

// PromLabels implements MetricsUpstream.
func (u *PromHTTPUpstream) PromLabels(ctx context.Context, _ string, match []string, start, end time.Time) ([]string, error) {
	form := url.Values{}
	for _, selector := range match {
		form.Add("match[]", selector)
	}
	setTimeRange(form, start, end)
	var response map[string]any
	if err := u.do(ctx, "/api/v1/labels", form, &response); err != nil {
		return nil, err
	}
	return ExtractPrometheusStringList(response), nil
}

// PromLabelValues implements MetricsUpstream.
func (u *PromHTTPUpstream) PromLabelValues(ctx context.Context, _, label string, match []string, start, end time.Time) ([]string, error) {
	form := url.Values{}
	for _, selector := range match {
		form.Add("match[]", selector)
	}
	setTimeRange(form, start, end)
	var response map[string]any
	if err := u.do(ctx, "/api/v1/label/"+url.PathEscape(label)+"/values", form, &response); err != nil {
		return nil, err
	}
	return ExtractPrometheusStringList(response), nil
}

func setTimeRange(form url.Values, start, end time.Time) {
	if !start.IsZero() {
		form.Set("start", strconv.FormatInt(start.Unix(), 10))
	}
	if !end.IsZero() {
		form.Set("end", strconv.FormatInt(end.Unix(), 10))
	}
}

func (u *PromHTTPUpstream) do(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	if u.tenant != "" {
		req.Header.Set("X-Scope-OrgID", u.tenant)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("prometheus %s: %w", u.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("prometheus %s: HTTP %d: %s", u.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return fmt.Errorf("prometheus %s: decode: %w", u.name, err)
	}
	return nil
}
