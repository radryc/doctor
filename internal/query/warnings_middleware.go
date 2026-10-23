package query

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/rydzu/ainfra/doctor/internal/federation"
	"github.com/rydzu/ainfra/doctor/internal/proxy"
)

// warningResponseWriter injects partial-failure headers from the request
// warning collector just before the response headers are committed. Handlers
// that already call writeProxyWarnings keep their richer header; the collector
// adds any warnings produced below the router layer (for example per-target
// federation failures).
type warningResponseWriter struct {
	http.ResponseWriter
	collector      *proxy.WarningCollector
	requestContext context.Context
	wrote          bool
}

func (w *warningResponseWriter) WriteHeader(statusCode int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.applyWarnings()
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *warningResponseWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *warningResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *warningResponseWriter) applyWarnings() {
	if w.collector == nil {
		return
	}
	if meta := federation.RequestMetaFromContext(w.requestContext); meta != nil {
		version, matched, selected := meta.HeaderValues()
		if version != "" {
			w.Header().Set("X-Doctor-Federation-Version", version)
		}
		if matched != "" {
			w.Header().Set("X-Doctor-Fanout-Matched", matched)
		}
		if selected != "" {
			w.Header().Set("X-Doctor-Fanout-Selected", selected)
		}
	}
	warnings := w.collector.Warnings()
	if len(warnings) == 0 {
		return
	}
	parts := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		parts = append(parts, warning.Backend+": "+warning.Message)
	}
	existing := strings.TrimSpace(w.Header().Get("X-Doctor-Partial"))
	if existing != "" {
		parts = append([]string{existing}, parts...)
	}
	w.Header().Set("X-Doctor-Partial", strings.Join(parts, "; "))
	count := w.collector.Total()
	if raw := strings.TrimSpace(w.Header().Get("X-Doctor-Partial-Count")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > count {
			count = parsed
		}
	}
	w.Header().Set("X-Doctor-Partial-Count", strconv.Itoa(count))
}

// withWarningHeaders installs a per-request warning collector and exposes its
// warnings as response headers even for handlers that do not return warnings
// explicitly.
func withWarningHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, collector := proxy.WithWarningCollector(r.Context())
		ctx, _ = federation.WithRequestMeta(ctx)
		next.ServeHTTP(&warningResponseWriter{ResponseWriter: w, collector: collector, requestContext: ctx}, r.WithContext(ctx))
	})
}
