package query

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/auditlog"
	"github.com/radryc/monofs/pkg/authz"
)

const (
	sharedDisplayHeader     = "X-Strata-Display-Profile"
	sharedDisplayQuery      = "displayProfile"
	defaultAuditEventsLimit = 100
	maxAuditEventsLimit     = 1000
	auditReadPathEndpoint   = "/v1/audit/events"
)

// AuditQuery filters audit events returned by GET /v1/audit/events.
type AuditQuery struct {
	From      time.Time
	To        time.Time
	Actor     string
	Action    string
	Partition string
	Decision  string
	Limit     int
}

// AuditReader reads audit events matching the given query.
type AuditReader interface {
	ReadAuditEvents(ctx context.Context, q AuditQuery) ([]map[string]any, bool, error)
}

type auditEventsResponse struct {
	Events    []map[string]any `json:"events"`
	Count     int              `json:"count"`
	Truncated bool             `json:"truncated,omitempty"`
}

type requestIDKey struct{}

// handleAuditStatus reports aggregate audit-pipeline health without exposing
// event content. It returns 503 when the store is unreachable, events are more
// than 15 minutes old, or a writer chain has conflicting sequences.
func (s *Service) handleAuditStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	reader, ok := s.auditReader.(StatusReader)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "audit status reader is not configured"})
		return
	}
	status, err := reader.Status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "audit status unavailable"})
		return
	}
	code := http.StatusOK
	if !status.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}

func requestIDFromContext(r *http.Request) string {
	if r == nil {
		return ""
	}
	value, _ := r.Context().Value(requestIDKey{}).(string)
	return value
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			var buf [12]byte
			if _, err := rand.Read(buf[:]); err == nil {
				requestID = hex.EncodeToString(buf[:])
			} else {
				requestID = time.Now().UTC().Format("20060102T150405.000000000")
			}
		}
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(contextWithRequestID(r, requestID)))
	})
}

func contextWithRequestID(r *http.Request, id string) context.Context {
	return context.WithValue(r.Context(), requestIDKey{}, id)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

func withAuditLogging(next http.Handler) http.Handler {
	return withAuditLoggingTo(next, "")
}

func withAuditLoggingTo(next http.Handler, logPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if r.Method == http.MethodHead || r.Method == http.MethodOptions {
			return
		}
		isAuditRead := r.URL.Path == auditReadPathEndpoint
		isReadQuery := auditReadPath(r.URL.Path)
		if r.Method == http.MethodGet && !isReadQuery && !isAuditRead {
			return
		}
		if r.Method != http.MethodGet && !auditPath(r.URL.Path) && !isReadQuery && !isAuditRead {
			return
		}
		identity, _ := authz.IdentityFromContext(r.Context())
		profile := strings.TrimSpace(r.Header.Get(sharedDisplayHeader))
		if profile == "" {
			profile = strings.TrimSpace(r.URL.Query().Get(sharedDisplayQuery))
		}
		event := map[string]any{
			"time":      time.Now().UTC().Format(time.RFC3339Nano),
			"requestId": requestIDFromContext(r),
			"method":    r.Method,
			"path":      r.URL.Path,
			"status":    recorder.status,
			"subject":   identity.Subject,
			"email":     identity.Email,
			"clientId":  identity.ClientID,
			"tenant":    strings.TrimSpace(r.URL.Query().Get("tenant")),
			"remote":    r.RemoteAddr,
			"detail":    "displayProfile=" + profile,
		}
		if isAuditRead {
			event["action"] = "audit_read"
		} else if isReadQuery {
			event["action"] = "read"
		}
		if err := writeAuditTo(logPath, event); err != nil {
			auditlog.IncrementWriteError("doctor-query")
		}
	})
}

func auditReadPath(path string) bool {
	for _, prefix := range []string{
		"/v1/logs", "/v1/traces", "/v1/sql/query",
		"/loki/api/v1/query", "/loki/api/v1/query_range",
		"/api/v1/query", "/v1/metrics/range", "/api/metrics/",
		"/api/v2/traces", "/api/search",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func auditPath(path string) bool {
	for _, prefix := range []string{
		"/v1/dashboards", "/v1/rules", "/v1/alerts", "/v1/federation",
		"/v1/incidents", "/v1/alert-sets", "/v1/incident-policy", "/v1/postmortem-engines",
		"/v1/guardian", "/v1/proxy", "/v1/services",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func writeAudit(event map[string]any) {
	if err := writeAuditTo("", event); err != nil {
		auditlog.IncrementWriteError("doctor-query")
	}
}

func writeAuditTo(explicitPath string, event map[string]any) error {
	return auditlog.WriteTo(explicitPath, event)
}

func (s *Service) effectiveAuditLogPath() string {
	if s != nil && strings.TrimSpace(s.auditLogPath) != "" {
		return strings.TrimSpace(s.auditLogPath)
	}
	return strings.TrimSpace(os.Getenv("DOCTOR_AUDIT_LOG"))
}

func (s *Service) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	identity, _ := authz.IdentityFromContext(r.Context())
	if s.policyGate != nil && !s.policyGate.CanReadAudit(identity) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: audit or admin scope required"})
		return
	}

	q, err := parseAuditQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var (
		events    []map[string]any
		truncated bool
	)
	if s.auditReader != nil {
		events, truncated, err = s.auditReader.ReadAuditEvents(r.Context(), q)
	} else {
		events, truncated, err = readAuditEventsFromFile(s.effectiveAuditLogPath(), q)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read audit log"})
		return
	}
	if events == nil {
		events = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, auditEventsResponse{
		Events:    events,
		Count:     len(events),
		Truncated: truncated,
	})
}

func parseAuditQuery(r *http.Request) (AuditQuery, error) {
	values := r.URL.Query()
	q := AuditQuery{
		Actor:     strings.TrimSpace(values.Get("actor")),
		Action:    strings.TrimSpace(values.Get("action")),
		Partition: strings.TrimSpace(values.Get("partition")),
		Decision:  strings.TrimSpace(values.Get("decision")),
		Limit:     parseLimit(strings.TrimSpace(values.Get("limit")), defaultAuditEventsLimit, maxAuditEventsLimit),
	}
	if rawFrom := strings.TrimSpace(values.Get("from")); rawFrom != "" {
		parsed, err := time.Parse(time.RFC3339, rawFrom)
		if err != nil {
			return AuditQuery{}, errors.New("invalid from timestamp: expected RFC3339")
		}
		q.From = parsed
	}
	if rawTo := strings.TrimSpace(values.Get("to")); rawTo != "" {
		parsed, err := time.Parse(time.RFC3339, rawTo)
		if err != nil {
			return AuditQuery{}, errors.New("invalid to timestamp: expected RFC3339")
		}
		q.To = parsed
	}
	if !q.From.IsZero() && !q.To.IsZero() && q.From.After(q.To) {
		return AuditQuery{}, errors.New("from must not be after to")
	}
	return q, nil
}

func readAuditEventsFromFile(path string, q AuditQuery) ([]map[string]any, bool, error) {
	if strings.TrimSpace(path) == "" {
		return []map[string]any{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []map[string]any{}, false, nil
		}
		return nil, false, err
	}
	defer file.Close()

	var matched []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if matchesAuditQuery(event, q) {
			matched = append(matched, event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}

	// Return newest events first so limit returns the most recent matches.
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}

	truncated := false
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[:q.Limit]
		truncated = true
	}
	if matched == nil {
		matched = []map[string]any{}
	}
	return matched, truncated, nil
}

func matchesAuditQuery(event map[string]any, q AuditQuery) bool {
	if !q.From.IsZero() || !q.To.IsZero() {
		ts, ok := auditEventTime(event)
		if !ok {
			return false
		}
		if !q.From.IsZero() && ts.Before(q.From) {
			return false
		}
		if !q.To.IsZero() && ts.After(q.To) {
			return false
		}
	}
	if q.Actor != "" && !matchesAuditActor(event, q.Actor) {
		return false
	}
	if q.Action != "" && !strings.EqualFold(auditStringField(event, "action"), q.Action) {
		return false
	}
	if q.Partition != "" && !strings.EqualFold(auditStringField(event, "partition"), q.Partition) {
		return false
	}
	if q.Decision != "" && !strings.EqualFold(auditStringField(event, "decision"), q.Decision) {
		return false
	}
	return true
}

func auditEventTime(event map[string]any) (time.Time, bool) {
	for _, key := range []string{"time", "timestamp", "ts"} {
		if raw := auditStringField(event, key); raw != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func matchesAuditActor(event map[string]any, actor string) bool {
	for _, key := range []string{"actor", "subject", "email", "clientId"} {
		if strings.EqualFold(auditStringField(event, key), actor) {
			return true
		}
	}
	return false
}

func auditStringField(event map[string]any, key string) string {
	val, ok := event[key]
	if !ok || val == nil {
		return ""
	}
	s, ok := val.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}
