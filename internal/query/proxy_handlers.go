package query

import (
	"net/http"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
)

// handlePrometheusQueryViaRouter executes an instant query through the
// multi-backend router, surfacing partial failures as a response header.
func (s *Service) handlePrometheusQueryViaRouter(w http.ResponseWriter, r *http.Request, query string, at time.Time) {
	data, warnings, err := s.proxyRouter.MetricsInstant(r.Context(), s.tenantFromRequest(r), query, at)
	if err != nil {
		if proxy.IsParseError(err) {
			writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		} else {
			writePrometheusExecutionError(w, err)
		}
		return
	}
	writeProxyWarnings(w, warnings)
	writePrometheusSuccess(w, data)
}

// handlePrometheusQueryRangeViaRouter executes a range query through the
// multi-backend router.
func (s *Service) handlePrometheusQueryRangeViaRouter(w http.ResponseWriter, r *http.Request, query string, start, end time.Time, step time.Duration) {
	data, warnings, err := s.proxyRouter.MetricsRange(r.Context(), s.tenantFromRequest(r), query, start, end, step)
	if err != nil {
		if proxy.IsParseError(err) {
			writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		} else {
			writePrometheusExecutionError(w, err)
		}
		return
	}
	writeProxyWarnings(w, warnings)
	writePrometheusSuccess(w, data)
}

// handleProxyEndpoints lists the configured backends and their health.
func (s *Service) handleProxyEndpoints(w http.ResponseWriter, _ *http.Request) {
	endpoints := []proxy.EndpointInfo{}
	if s.proxyRouter != nil {
		endpoints = s.proxyRouter.Endpoints()
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": endpoints})
}

// writeProxyWarnings reports partial backend failures without failing the
// response.
func writeProxyWarnings(w http.ResponseWriter, warnings []proxy.Warning) {
	if len(warnings) == 0 {
		return
	}
	parts := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		parts = append(parts, warning.Backend+": "+warning.Message)
	}
	w.Header().Set("X-Doctor-Partial", strings.Join(parts, "; "))
}
