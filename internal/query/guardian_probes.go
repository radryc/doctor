package query

import (
	"io"
	"net/http"
	"strings"
)

// handleGuardianProbes proxies Guardian's synthetic probe projection.
//
// Guardian owns probe execution and persistence; Doctor only presents the
// latest snapshots. Keeping this as a thin proxy means the Guardian ACL is
// evaluated with the caller's own token (guardianTokenMiddleware forwards it)
// rather than with Doctor's service identity.
func (s *Service) handleGuardianProbes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	base := s.guardianAPIBaseURL()
	if base == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guardian url not configured"})
		return
	}

	target := strings.TrimRight(base, "/") + "/api/probes"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to build guardian probes request"})
		return
	}
	req.Header.Set("Accept", "application/json")
	if token := guardianTokenFromContext(r.Context()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := s.guardianClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "guardian unreachable"})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to read guardian response"})
		return
	}
	if contentType := strings.TrimSpace(resp.Header.Get("Content-Type")); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
