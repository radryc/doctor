// Package auditlog writes structured audit events to the shared JSONL sink and
// provides a minimal middleware for mutation/admin endpoints in services that
// do not use the full doctor-query audit pipeline.
package auditlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/radryc/monofs/pkg/audit"
	"github.com/radryc/monofs/pkg/authz"
)

// RequestIDHeader is the canonical correlation header.
const RequestIDHeader = "X-Request-ID"

var writeErrors = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "doctor_audit_write_errors_total",
	Help: "Audit events that could not be persisted to the configured JSONL sink.",
}, []string{"component"})

var (
	auditMu       sync.Mutex
	auditFile     *os.File
	auditFileMu   sync.Mutex
	auditFilePath string
	emitterOnce   sync.Once
	sharedEmitter *audit.ServiceEmitter
)

// IncrementWriteError records one failed audit write for the component.
func IncrementWriteError(component string) {
	if strings.TrimSpace(component) == "" {
		component = "unknown"
	}
	writeErrors.WithLabelValues(component).Inc()
}

// Write emits an event to DOCTOR_AUDIT_LOG, or to the standard audit log when
// no file is configured.
func Write(event map[string]any) error {
	return WriteTo("", event)
}

// WriteTo emits an event to an explicit path, falling back to the
// DOCTOR_AUDIT_LOG environment variable. Failures are logged with the request
// ID carried in the event and returned so callers can increment metrics.
func WriteTo(explicitPath string, event map[string]any) error {
	payload, err := json.Marshal(event)
	if err != nil {
		log.Printf("audit_log_marshal_error requestId=%q err=%v", stringField(event, "requestId"), err)
		return err
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	path := strings.TrimSpace(explicitPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("DOCTOR_AUDIT_LOG"))
	}
	requestID := stringField(event, "requestId")
	if path != "" {
		auditFileMu.Lock()
		defer auditFileMu.Unlock()
		if auditFile == nil || auditFilePath != path {
			if auditFile != nil {
				_ = auditFile.Close()
			}
			file, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if openErr != nil {
				log.Printf("audit_log_open_error requestId=%q path=%q err=%v audit=%s", requestID, path, openErr, payload)
				return openErr
			}
			auditFile = file
			auditFilePath = path
		}
		if _, writeErr := auditFile.Write(append(payload, '\n')); writeErr != nil {
			log.Printf("audit_log_write_error requestId=%q path=%q err=%v audit=%s", requestID, path, writeErr, payload)
			return writeErr
		}
		emitShared(event)
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DOCTOR_AUDIT_ENABLED")), "false") {
		return nil
	}
	log.Printf("audit %s", payload)
	emitShared(event)
	return nil
}

// sharedEmitter returns the process-wide ClickHouse emitter configured by the
// AUDIT_* environment contract. It is nil when auditing is disabled.
func sharedEmitterInstance() *audit.ServiceEmitter {
	emitterOnce.Do(func() {
		emitter, err := audit.NewServiceEmitter(audit.ConfigFromEnv("doctor"))
		if err != nil {
			log.Printf("audit_emitter_error err=%v", err)
			return
		}
		sharedEmitter = emitter
	})
	return sharedEmitter
}

func emitShared(event map[string]any) {
	emitter := sharedEmitterInstance()
	if emitter == nil {
		return
	}
	action := audit.ActionWrite
	if method, _ := event["method"].(string); method == http.MethodGet || method == http.MethodHead {
		action = audit.ActionRead
	}
	if raw, _ := event["action"].(string); raw == "read" || raw == "audit_read" {
		action = audit.Action(raw)
	}
	decision := audit.DecisionAllow
	status := intField(event, "status")
	if status >= http.StatusBadRequest {
		decision = audit.DecisionDeny
	}
	service, _ := event["component"].(string)
	if service == "" {
		service = "doctor"
	}
	ev := audit.Event{
		Service:       service,
		Scope:         "telemetry",
		Action:        action,
		Decision:      decision,
		Reason:        eventString(event, "reason"),
		ActorSubject:  eventString(event, "subject"),
		ActorClientID: eventString(event, "clientId"),
		Email:         eventString(event, "email"),
		Tenant:        eventString(event, "tenant"),
		Resource:      eventString(event, "path"),
		ResourceType:  "http",
		RequestID:     eventString(event, "requestId"),
		Method:        eventString(event, "method"),
		Path:          eventString(event, "path"),
		Status:        status,
		SourceIP:      eventString(event, "remote"),
		UserAgent:     eventString(event, "userAgent"),
		Detail:        eventString(event, "detail"),
	}
	_ = emitter.Emit(context.Background(), ev)
}

func eventString(event map[string]any, key string) string {
	value, _ := event[key].(string)
	return strings.TrimSpace(value)
}

func intField(event map[string]any, key string) int {
	switch v := event[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// MutationMiddleware audits non-GET requests whose path matches one of the
// supplied prefixes. A nil/empty prefix list audits every mutating request,
// which is only appropriate for control-plane APIs. It never changes the
// wrapped response.
func MutationMiddleware(component string, prefixes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isMutationMethod(r.Method) || !pathMatches(r.URL.Path, prefixes) {
				next.ServeHTTP(w, r)
				return
			}
			recorder := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r)

			requestID := strings.TrimSpace(r.Header.Get(RequestIDHeader))
			if requestID == "" {
				requestID = newRequestID()
			}
			identity, _ := authz.IdentityFromContext(r.Context())
			user := identity.Subject
			if user == "" {
				user = identity.Email
			}
			if user == "" {
				user = identity.ClientID
			}
			if user == "" {
				user = strings.TrimSpace(r.Header.Get("X-Doctor-User"))
			}
			event := map[string]any{
				"time":      time.Now().UTC().Format(time.RFC3339Nano),
				"requestId": requestID,
				"component": component,
				"action":    "mutation",
				"method":    r.Method,
				"path":      r.URL.Path,
				"status":    recorder.status,
				"subject":   identity.Subject,
				"email":     identity.Email,
				"clientId":  identity.ClientID,
				"user":      user,
				"tenant":    strings.TrimSpace(r.URL.Query().Get("tenant")),
				"remote":    r.RemoteAddr,
			}
			if err := WriteTo("", event); err != nil {
				IncrementWriteError(component)
			}
		})
	}
}

func isMutationMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func pathMatches(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func stringField(event map[string]any, key string) string {
	value, _ := event[key].(string)
	return value
}

func newRequestID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	return time.Now().UTC().Format("20060102T150405.000000000")
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
