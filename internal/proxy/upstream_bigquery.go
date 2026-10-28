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

	"github.com/radryc/doctor/internal/proxy/translate"
	"github.com/radryc/doctor/internal/segment"
)

const defaultBigQueryBaseURL = "https://bigquery.googleapis.com/bigquery/v2"

// BigQueryUpstream queries BigQuery with read-only SQL. Only the selector and
// line filters are translated; parser stages, label filters, and metric
// aggregation run locally after merge.
type BigQueryUpstream struct {
	name    string
	baseURL string
	token   string
	project string
	dataset string
	table   string
	schema  map[string]string
	client  *http.Client
}

// NewBigQueryUpstream builds a BigQuery adapter for a backend.
func NewBigQueryUpstream(backend Backend, client *http.Client) *BigQueryUpstream {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	baseURL := strings.TrimRight(backend.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBigQueryBaseURL
	}
	return &BigQueryUpstream{
		name:    backend.Name,
		baseURL: baseURL,
		token:   backend.Token,
		project: backend.Project,
		dataset: backend.Dataset,
		table:   backend.Table,
		schema:  backend.Schema,
		client:  client,
	}
}

// Name implements LogsUpstream.
func (u *BigQueryUpstream) Name() string { return u.name }

// QueryLogs implements LogsUpstream.
func (u *BigQueryUpstream) QueryLogs(ctx context.Context, tenant string, pushdown LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	if u.project == "" {
		return nil, fmt.Errorf("%w: bigquery backend requires a project", translate.ErrUnsupported)
	}
	sql, err := translate.LogQLToBigQuery(translate.LogQuery{
		Selector:    pushdown.Selector,
		LineFilters: lineFiltersToTranslate(pushdown.LineFilters),
	}, translate.BQConfig{
		Project: u.project,
		Dataset: u.dataset,
		Table:   u.table,
		Schema:  u.schema,
		From:    from,
		To:      to,
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]any{
		"query":        sql,
		"useLegacySql": false,
		"maxResults":   limit,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+"/projects/"+u.project+"/queries", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bigquery %s: %w", u.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("bigquery %s: HTTP %d: %s", u.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result bigQueryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("bigquery %s: decode: %w", u.name, err)
	}
	rows := result.Rows
	fields := result.Schema.Fields
	pageToken := result.PageToken
	jobID := result.JobReference.JobID
	for pageToken != "" && jobID != "" {
		page, err := u.fetchPage(ctx, jobID, pageToken)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.Rows...)
		if len(page.Schema.Fields) > 0 {
			fields = page.Schema.Fields
		}
		pageToken = page.PageToken
	}
	return recordsFromBigQuery(rows, fields, tenant, u.schema), nil
}

func (u *BigQueryUpstream) fetchPage(ctx context.Context, jobID, pageToken string) (bigQueryResponse, error) {
	target := fmt.Sprintf("%s/projects/%s/queries/%s?pageToken=%s", u.baseURL, u.project, jobID, pageToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return bigQueryResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return bigQueryResponse{}, fmt.Errorf("bigquery %s: %w", u.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return bigQueryResponse{}, fmt.Errorf("bigquery %s: HTTP %d: %s", u.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var page bigQueryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&page); err != nil {
		return bigQueryResponse{}, fmt.Errorf("bigquery %s: decode page: %w", u.name, err)
	}
	return page, nil
}

type bigQueryResponse struct {
	JobComplete  bool `json:"jobComplete"`
	JobReference struct {
		JobID string `json:"jobId"`
	} `json:"jobReference"`
	Schema struct {
		Fields []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"fields"`
	} `json:"schema"`
	Rows []struct {
		F []struct {
			V any `json:"v"`
		} `json:"f"`
	} `json:"rows"`
	PageToken string `json:"pageToken"`
}

func recordsFromBigQuery(rows []struct {
	F []struct {
		V any `json:"v"`
	} `json:"f"`
}, fields []struct {
	Name string `json:"name"`
	Type string `json:"type"`
}, tenant string, schema map[string]string) []segment.LogRecord {
	if len(fields) == 0 {
		return nil
	}
	mapped := translate.DefaultBQSchema()
	for key, value := range schema {
		if strings.TrimSpace(value) != "" {
			mapped[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}
	index := make(map[string]int, len(fields))
	for i, field := range fields {
		index[field.Name] = i
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

	records := make([]segment.LogRecord, 0, len(rows))
	for _, row := range rows {
		record := segment.LogRecord{Tenant: tenant}
		if timestampIdx < len(row.F) {
			record.Timestamp = bqTime(row.F[timestampIdx].V)
		}
		if bodyIdx < len(row.F) {
			record.Body = bqStringValue(row.F[bodyIdx].V)
		}
		if hasService && serviceIdx < len(row.F) {
			record.Service = bqStringValue(row.F[serviceIdx].V)
		}
		if hasLevel && levelIdx < len(row.F) {
			record.SeverityText = bqStringValue(row.F[levelIdx].V)
		}
		if hasTrace && traceIdx < len(row.F) {
			record.TraceID = bqStringValue(row.F[traceIdx].V)
		}
		if hasSpan && spanIdx < len(row.F) {
			record.SpanID = bqStringValue(row.F[spanIdx].V)
		}
		attributes := make(map[string]string)
		for i, field := range fields {
			if i >= len(row.F) {
				continue
			}
			switch field.Name {
			case mapped["timestamp"], mapped["body"], mapped["service"], mapped["level"], mapped["trace_id"], mapped["span_id"]:
				continue
			}
			if value := bqStringValue(row.F[i].V); value != "" {
				attributes[field.Name] = value
			}
		}
		if len(attributes) > 0 {
			record.Attributes = attributes
		}
		records = append(records, record)
	}
	return records
}

func bqStringValue(value any) string {
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

func bqTime(value any) time.Time {
	switch typed := value.(type) {
	case string:
		if seconds, err := strconv.ParseFloat(typed, 64); err == nil {
			return time.Unix(0, int64(seconds*float64(time.Second))).UTC()
		}
		if parsed, err := time.Parse(time.RFC3339Nano, typed); err == nil {
			return parsed.UTC()
		}
	case float64:
		return time.Unix(0, int64(typed*float64(time.Second))).UTC()
	}
	return time.Time{}
}
