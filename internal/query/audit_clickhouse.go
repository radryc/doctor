package query

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ClickHouseAuditReader reads audit events from the shared audit.events table.
// The table is append-only and owned by the emitters; this reader never writes.
type ClickHouseAuditReader struct {
	endpoint string
	database string
	user     string
	password string
	client   *http.Client
}

var _ AuditReader = (*ClickHouseAuditReader)(nil)

// NewClickHouseAuditReaderFromEnv returns a reader when DOCTOR_AUDIT_CH_URL is
// configured, otherwise nil (the JSONL reader remains the local-dev fallback).
func NewClickHouseAuditReaderFromEnv() *ClickHouseAuditReader {
	endpoint := strings.TrimSpace(os.Getenv("DOCTOR_AUDIT_CH_URL"))
	if endpoint == "" {
		return nil
	}
	database := strings.TrimSpace(os.Getenv("DOCTOR_AUDIT_CH_DATABASE"))
	if database == "" {
		database = "audit"
	}
	return &ClickHouseAuditReader{
		endpoint: strings.TrimRight(endpoint, "/"),
		database: database,
		user:     strings.TrimSpace(os.Getenv("DOCTOR_AUDIT_CH_USER")),
		password: os.Getenv("DOCTOR_AUDIT_CH_PASSWORD"),
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// AuditStatus is the aggregate health snapshot used by the status endpoint and
// the Guardian probe. It never includes event content.
type AuditStatus struct {
	OK                   bool       `json:"ok"`
	ClickHouse           bool       `json:"clickhouse"`
	EventsTotal          uint64     `json:"eventsTotal"`
	EventsRecent         uint64     `json:"eventsRecent"`
	LastEventAt          *time.Time `json:"lastEventAt,omitempty"`
	ConflictingSequences uint64     `json:"conflictingSequences"`
	MaxEventAge          string     `json:"maxEventAge"`
}

// StatusReader is implemented by readers that can report aggregate health.
type StatusReader interface {
	Status(ctx context.Context) (AuditStatus, error)
}

// Status returns aggregate counts and recent-event age without exposing events.
func (r *ClickHouseAuditReader) Status(ctx context.Context) (AuditStatus, error) {
	status := AuditStatus{ClickHouse: true, MaxEventAge: "15m"}
	if r == nil || r.endpoint == "" {
		return status, fmt.Errorf("audit reader is not configured")
	}
	var total uint64
	var recent uint64
	var last time.Time
	query := "SELECT count() AS total, countIf(timestamp >= now() - INTERVAL 15 MINUTE) AS recent, max(timestamp) AS last FROM " + r.database + ".events FORMAT JSONEachRow"
	row, err := r.queryOne(ctx, query)
	if err != nil {
		return status, err
	}
	if value, ok := row["total"].(float64); ok {
		total = uint64(value)
	}
	if value, ok := row["recent"].(float64); ok {
		recent = uint64(value)
	}
	if raw, ok := row["last"].(string); ok && raw != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			last = parsed
		}
	}
	conflictsQuery := "SELECT count() AS conflicts FROM (SELECT writer_id, seq, uniqExact(hash) h FROM " + r.database + ".events GROUP BY writer_id, seq HAVING h > 1) FORMAT JSONEachRow"
	conflictRow, err := r.queryOne(ctx, conflictsQuery)
	if err != nil {
		return status, err
	}
	var conflicts uint64
	if value, ok := conflictRow["conflicts"].(float64); ok {
		conflicts = uint64(value)
	}
	status.EventsTotal = total
	status.EventsRecent = recent
	status.ConflictingSequences = conflicts
	if !last.IsZero() {
		status.LastEventAt = &last
	}
	status.OK = conflicts == 0 && !last.IsZero() && time.Since(last) <= 15*time.Minute
	return status, nil
}

func (r *ClickHouseAuditReader) queryOne(ctx context.Context, query string) (map[string]any, error) {
	u, err := url.Parse(r.endpoint)
	if err != nil {
		return nil, err
	}
	params := u.Query()
	params.Set("query", query)
	params.Set("date_time_output_format", "iso")
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if r.user != "" || r.password != "" {
		req.SetBasicAuth(r.user, r.password)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("audit status query: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var row map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&row); err != nil {
		return nil, err
	}
	return row, nil
}

// ReadAuditEvents implements AuditReader.
func (r *ClickHouseAuditReader) ReadAuditEvents(ctx context.Context, q AuditQuery) ([]map[string]any, bool, error) {
	if r == nil || r.endpoint == "" {
		return []map[string]any{}, false, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > maxAuditEventsLimit {
		limit = defaultAuditEventsLimit
	}
	where := make([]string, 0, 6)
	if !q.From.IsZero() {
		where = append(where, "timestamp >= toDateTime64('"+q.From.UTC().Format("2006-01-02 15:04:05.000000")+"', 6, 'UTC')")
	}
	if !q.To.IsZero() {
		where = append(where, "timestamp <= toDateTime64('"+q.To.UTC().Format("2006-01-02 15:04:05.000000")+"', 6, 'UTC')")
	}
	if q.Actor != "" {
		actor := sqlString(q.Actor)
		where = append(where, "(actor_subject = "+actor+" OR email = "+actor+" OR actor_client_id = "+actor+")")
	}
	if q.Action != "" {
		where = append(where, "action = "+sqlString(q.Action))
	}
	if q.Partition != "" {
		where = append(where, "partition = "+sqlString(q.Partition))
	}
	if q.Decision != "" {
		where = append(where, "decision = "+sqlString(q.Decision))
	}
	query := "SELECT timestamp, event_id, writer_id, seq, service, env, scope, action, decision, reason, " +
		"resource_type, actor_subject, actor_client_id, email, groups, tenant, partition, resource, " +
		"request_id, trace_id, method, path, status, result_count, bytes, occurrences, source_ip, " +
		"user_agent, policy_version, detail " +
		"FROM " + r.database + ".events"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY timestamp DESC LIMIT %d FORMAT JSONEachRow", limit+1)

	u, err := url.Parse(r.endpoint)
	if err != nil {
		return nil, false, err
	}
	params := u.Query()
	params.Set("query", query)
	params.Set("date_time_output_format", "iso")
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, false, err
	}
	if r.user != "" || r.password != "" {
		req.SetBasicAuth(r.user, r.password)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("audit clickhouse query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, false, fmt.Errorf("audit clickhouse query: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	events := make([]map[string]any, 0, limit)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, false, fmt.Errorf("decode audit row: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(events) > limit
	if truncated {
		events = events[:limit]
	}
	return events, truncated, nil
}

func sqlString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `'`, `\'`)
	return "'" + value + "'"
}
