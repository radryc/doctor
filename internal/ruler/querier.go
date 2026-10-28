package ruler

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

	"github.com/radryc/doctor/internal/proxy"
)

// HTTPQuerier executes PromQL through doctor-query's Prometheus API.
type HTTPQuerier struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewHTTPQuerier builds a querier for a doctor-query base URL.
func NewHTTPQuerier(baseURL, token string, client *http.Client) *HTTPQuerier {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPQuerier{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		Client:  client,
	}
}

// PromInstant implements Querier.
func (q *HTTPQuerier) PromInstant(ctx context.Context, query string, at time.Time) (map[string]any, bool, error) {
	if q.BaseURL == "" {
		return nil, false, fmt.Errorf("ruler: query URL is not configured")
	}
	form := url.Values{}
	form.Set("query", query)
	form.Set("time", strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.BaseURL+"/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if q.Token != "" {
		req.Header.Set("Authorization", "Bearer "+q.Token)
	}
	resp, err := q.Client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("ruler: query doctor-query: %w", err)
	}
	defer resp.Body.Close()
	partial := resp.Header.Get("X-Doctor-Partial") != ""
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, partial, fmt.Errorf("ruler: query HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var response map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&response); err != nil {
		return nil, partial, fmt.Errorf("ruler: decode query response: %w", err)
	}
	if status, _ := response["status"].(string); status != "" && status != "success" {
		return nil, partial, fmt.Errorf("ruler: query failed: %v", response["error"])
	}
	return proxy.ExtractPrometheusData(response), partial, nil
}
