package query

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const probeStaleAfter = 3 * time.Minute

type probePusherSummary struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Down      int `json:"down"`
}

type probeEnvelope struct {
	Probes []json.RawMessage `json:"probes"`
}

// summarizeProbePushers groups the latest probe results by pusher. A pusher is
// available when at least one of its probes was checked inside the freshness
// window; otherwise it is counted as down. The response never lists pusher
// names, so a large fleet cannot flood the UI.
func summarizeProbePushers(probes []json.RawMessage, now time.Time) probePusherSummary {
	freshByPusher := map[string]bool{}
	for _, raw := range probes {
		var meta struct {
			Pusher    string    `json:"pusher"`
			CheckedAt time.Time `json:"checkedAt"`
		}
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		pusher := strings.TrimSpace(meta.Pusher)
		if pusher == "" {
			pusher = "unknown"
		}
		if _, ok := freshByPusher[pusher]; !ok {
			freshByPusher[pusher] = false
		}
		if !meta.CheckedAt.IsZero() && now.Sub(meta.CheckedAt) <= probeStaleAfter {
			freshByPusher[pusher] = true
		}
	}
	summary := probePusherSummary{Total: len(freshByPusher)}
	for _, fresh := range freshByPusher {
		if fresh {
			summary.Available++
		} else {
			summary.Down++
		}
	}
	return summary
}

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
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var envelope probeEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid guardian probes response"})
			return
		}
		kept := envelope.Probes
		if s.policyGate != nil && s.policyGate.Enforced() {
			id := identityFromContext(r.Context())
			filtered := make([]json.RawMessage, 0, len(kept))
			for _, raw := range kept {
				var meta struct {
					Partition string `json:"partition"`
				}
				if json.Unmarshal(raw, &meta) != nil {
					continue
				}
				if s.policyGate.CanSeePartition(id, meta.Partition) {
					filtered = append(filtered, raw)
				}
			}
			kept = filtered
		}
		body, err = json.Marshal(map[string]any{
			"probes":        kept,
			"pusherSummary": summarizeProbePushers(kept, time.Now().UTC()),
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to encode probes"})
			return
		}
	}
	if contentType := strings.TrimSpace(resp.Header.Get("Content-Type")); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
