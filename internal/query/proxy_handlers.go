package query

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/access"
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
func (s *Service) handleProxyEndpoints(w http.ResponseWriter, r *http.Request) {
	endpoints := []proxy.EndpointInfo{}
	if s.proxyRouter != nil {
		endpoints = s.proxyRouter.Endpoints(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": endpoints})
}

// writeProxyWarnings reports partial backend failures without failing the
// response. Observed access denials are surfaced separately so operators can
// audit the observe-first rollout.
func writeProxyWarnings(w http.ResponseWriter, warnings []proxy.Warning) {
	if len(warnings) == 0 {
		return
	}
	parts := make([]string, 0, len(warnings))
	observed := 0
	for _, warning := range warnings {
		parts = append(parts, warning.Backend+": "+warning.Message)
		if strings.Contains(warning.Message, "access denied (observed)") {
			observed++
		}
	}
	w.Header().Set("X-Doctor-Partial", strings.Join(parts, "; "))
	if observed > 0 {
		w.Header().Set("X-Doctor-Access-Observed", strconv.Itoa(observed))
	}
}

// isAccessDenied reports whether an upstream error is an access enforcement
// denial.
func isAccessDenied(err error) bool {
	return errors.Is(err, access.ErrAccessDenied)
}
