package query

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

	"github.com/radryc/monofs/pkg/authz"
)

type requestIDKey struct{}

var (
	auditMu       sync.Mutex
	auditFile     *os.File
	auditFileMu   sync.Mutex
	auditFilePath string
)

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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			return
		}
		if !auditPath(r.URL.Path) {
			return
		}
		identity, _ := authz.IdentityFromContext(r.Context())
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
		}
		writeAudit(event)
	})
}

func auditPath(path string) bool {
	for _, prefix := range []string{
		"/v1/dashboards", "/v1/rules", "/v1/alerts", "/v1/federation",
		"/v1/guardian", "/v1/proxy", "/v1/services",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func writeAudit(event map[string]any) {
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	if path := strings.TrimSpace(os.Getenv("DOCTOR_AUDIT_LOG")); path != "" {
		auditFileMu.Lock()
		defer auditFileMu.Unlock()
		if auditFile == nil || auditFilePath != path {
			if auditFile != nil {
				_ = auditFile.Close()
			}
			file, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if openErr != nil {
				return
			}
			auditFile = file
			auditFilePath = path
		}
		_, _ = auditFile.Write(append(payload, '\n'))
		return
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DOCTOR_AUDIT_ENABLED")), "false") {
		return
	}
	log.Printf("audit %s", payload)
}
