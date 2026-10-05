package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy/translate"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// ElasticsearchUpstream queries an Elasticsearch cluster through the ES|QL
// `_query` API. Only the selector and line filters are translated; parser
// stages, label filters, and metric aggregation run locally after merge.
type ElasticsearchUpstream struct {
	name    string
	baseURL string
	token   string
	index   string
	schema  map[string]string
	client  *http.Client
}

// NewElasticsearchUpstream builds an ES|QL adapter for a backend.
func NewElasticsearchUpstream(backend Backend, client *http.Client) *ElasticsearchUpstream {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &ElasticsearchUpstream{
		name:    backend.Name,
		baseURL: strings.TrimRight(backend.BaseURL, "/"),
		token:   backend.Token,
		index:   backend.Index,
		schema:  backend.Schema,
		client:  client,
	}
}

// Name implements LogsUpstream.
func (u *ElasticsearchUpstream) Name() string { return u.name }

// QueryLogs implements LogsUpstream.
func (u *ElasticsearchUpstream) QueryLogs(ctx context.Context, tenant string, pushdown LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	query, err := translate.LogQLToESQL(translate.LogQuery{
		Selector:    pushdown.Selector,
		LineFilters: lineFiltersToTranslate(pushdown.LineFilters),
	}, translate.ESConfig{
		Index:  u.index,
		Schema: u.schema,
		From:   from,
		To:     to,
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+"/_query?format=json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	if orgID := firstNonEmpty(tenant, ""); orgID != "" && orgID != "default" {
		req.Header.Set("X-Scope-OrgID", orgID)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch %s: %w", u.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("elasticsearch %s: HTTP %d: %s", u.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result esqlResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("elasticsearch %s: decode: %w", u.name, err)
	}
	return recordsFromESQL(result, tenant, u.name, u.schema), nil
}

func lineFiltersToTranslate(filters []LogLineFilter) []translate.LogLineFilter {
	out := make([]translate.LogLineFilter, 0, len(filters))
	for _, filter := range filters {
		out = append(out, translate.LogLineFilter{Op: filter.Op, Value: filter.Value})
	}
	return out
}

type esqlResponse struct {
	Columns []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"columns"`
	Values [][]any `json:"values"`
}

func recordsFromESQL(response esqlResponse, tenant, backend string, schema map[string]string) []segment.LogRecord {
	if len(response.Columns) == 0 {
		return nil
	}
	mapped := translate.DefaultESSchema()
	for key, value := range schema {
		if strings.TrimSpace(value) != "" {
			mapped[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}
	index := make(map[string]int, len(response.Columns))
	for i, column := range response.Columns {
		index[column.Name] = i
	}
	timestampIdx, hasTimestamp := index[mapped["timestamp"]]
	bodyIdx, hasBody := index[mapped["body"]]
	serviceIdx, hasService := index[mapped["service"]]
	levelIdx, hasLevel := index[mapped["level"]]
	traceIdx, hasTrace := index[mapped["trace_id"]]
	spanIdx, hasSpan := index[mapped["span_id"]]
	if !hasTimestamp || !hasBody {
		return nil
	}

	records := make([]segment.LogRecord, 0, len(response.Values))
	for _, row := range response.Values {
		record := segment.LogRecord{
			Tenant:    tenant,
			Timestamp: esTime(row[timestampIdx]),
			Body:      esStringValue(row[bodyIdx]),
		}
		if hasService {
			record.Service = esStringValue(row[serviceIdx])
		}
		if hasLevel {
			record.SeverityText = esStringValue(row[levelIdx])
		}
		if hasTrace {
			record.TraceID = esStringValue(row[traceIdx])
		}
		if hasSpan {
			record.SpanID = esStringValue(row[spanIdx])
		}
		attributes := make(map[string]string)
		for i, column := range response.Columns {
			if i >= len(row) {
				continue
			}
			switch column.Name {
			case mapped["timestamp"], mapped["body"], mapped["service"], mapped["level"], mapped["trace_id"], mapped["span_id"]:
				continue
			}
			if value := esStringValue(row[i]); value != "" {
				attributes[column.Name] = value
			}
		}
		if len(attributes) > 0 {
			record.Attributes = attributes
		}
		records = append(records, record)
	}
	return records
}

func esStringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}

func esTime(value any) time.Time {
	switch typed := value.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, typed); err == nil {
			return parsed.UTC()
		}
		if millis, err := strconv.ParseInt(typed, 10, 64); err == nil {
			return time.UnixMilli(millis).UTC()
		}
	case float64:
		return time.UnixMilli(int64(typed)).UTC()
	}
	return time.Time{}
}
