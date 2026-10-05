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

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// LokiHTTPUpstream queries a remote Loki-compatible HTTP API.
type LokiHTTPUpstream struct {
	name    string
	baseURL string
	token   string
	tenant  string
	client  *http.Client
}

// NewLokiHTTPUpstream builds a Loki HTTP adapter for a backend.
func NewLokiHTTPUpstream(backend Backend, client *http.Client) *LokiHTTPUpstream {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &LokiHTTPUpstream{
		name:    backend.Name,
		baseURL: strings.TrimRight(backend.BaseURL, "/"),
		token:   backend.Token,
		tenant:  backend.Tenant,
		client:  client,
	}
}

// Name implements LogsUpstream.
func (u *LokiHTTPUpstream) Name() string { return u.name }

// QueryLogs implements LogsUpstream.
func (u *LokiHTTPUpstream) QueryLogs(ctx context.Context, tenant string, pushdown LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	form := url.Values{}
	form.Set("query", pushdown.Query())
	form.Set("start", strconv.FormatInt(from.UTC().UnixNano(), 10))
	form.Set("end", strconv.FormatInt(to.UTC().UnixNano(), 10))
	form.Set("limit", strconv.Itoa(limit))
	form.Set("direction", "backward")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+"/loki/api/v1/query_range", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	if orgID := firstNonEmpty(tenant, u.tenant); orgID != "" && orgID != "default" {
		req.Header.Set("X-Scope-OrgID", orgID)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki %s: %w", u.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("loki %s: HTTP %d: %s", u.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var response lokiQueryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&response); err != nil {
		return nil, fmt.Errorf("loki %s: decode: %w", u.name, err)
	}
	return lokiRecordsFromResponse(response, tenant, u.name), nil
}

// Query renders the storage subset of a LogQL query (selector plus line
// filters) for backends that cannot evaluate parser stages.
func (p LogPushdown) Query() string {
	var builder strings.Builder
	builder.WriteString(strings.TrimSpace(p.Selector))
	for _, filter := range p.LineFilters {
		builder.WriteByte(' ')
		builder.WriteString(filter.Op)
		builder.WriteByte(' ')
		builder.WriteString(strconv.Quote(filter.Value))
	}
	return builder.String()
}

type lokiQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func lokiRecordsFromResponse(response lokiQueryResponse, tenant, backend string) []segment.LogRecord {
	records := make([]segment.LogRecord, 0)
	for _, stream := range response.Data.Result {
		service := firstNonEmpty(stream.Stream["service"], stream.Stream["service_name"], backend)
		for _, value := range stream.Values {
			if len(value) < 2 {
				continue
			}
			nanos, err := strconv.ParseInt(value[0], 10, 64)
			if err != nil {
				continue
			}
			attributes := make(map[string]string, len(stream.Stream))
			for key, label := range stream.Stream {
				if key == "service" || key == "service_name" {
					continue
				}
				attributes[key] = label
			}
			records = append(records, segment.LogRecord{
				Tenant:     tenant,
				Service:    service,
				Timestamp:  time.Unix(0, nanos).UTC(),
				Body:       value[1],
				Attributes: attributes,
			})
		}
	}
	return records
}
