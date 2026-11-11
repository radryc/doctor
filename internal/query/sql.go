package query

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	monofsstore "github.com/radryc/doctor/internal/store/monofs"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

const (
	defaultDuckDBSQLRows = 1_000
	maxDuckDBSQLRows     = 5_000
	maxDuckDBSQLRange    = 7 * 24 * time.Hour
)

type duckDBSQLRequest struct {
	Dataset string `json:"dataset"`
	Query   string `json:"query"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type duckDBSQLResponse struct {
	Dataset           string                            `json:"dataset"`
	Nodes             []monofsstore.DuckDBSQLNodeResult `json:"nodes"`
	Restricted        bool                              `json:"restricted"`
	VisiblePartitions []string                          `json:"visible_partitions,omitempty"`
}

// handleDuckDBSQL serves POST /v1/sql/query. It is a read-only SQL console
// over the MonoFS DuckDB views; partition scope is resolved from the verified
// caller identity before the request reaches MonoFS.
func (s *Service) handleDuckDBSQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req duckDBSQLRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return
	}
	req.Dataset = strings.ToLower(strings.TrimSpace(req.Dataset))
	switch req.Dataset {
	case "logs", "metrics", "traces":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dataset must be logs, metrics, or traces"})
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query is required"})
		return
	}
	if len(req.Query) > 32*1024 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query exceeds the 32 KiB limit"})
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultDuckDBSQLRows
	}
	if limit > maxDuckDBSQLRows {
		limit = maxDuckDBSQLRows
	}
	now := time.Now().UTC()
	to, err := parseSQLTime(req.To, now)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid to time"})
		return
	}
	from, err := parseSQLTime(req.From, to.Add(-time.Hour))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid from time"})
		return
	}
	if !to.After(from) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from must be before to"})
		return
	}
	if to.Sub(from) > maxDuckDBSQLRange {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "time range is limited to 7 days"})
		return
	}

	visible, restricted := s.duckDBScope(r.Context(), req.Dataset)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result, err := s.logEngine.QueryDuckDBSQL(ctx, s.tenantFromRequest(r), req.Dataset, req.Query, from, to, limit, visible, restricted)
	if err != nil {
		writeJSON(w, duckDBSQLErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	resp := duckDBSQLResponse{
		Dataset:           req.Dataset,
		Nodes:             result.Nodes,
		Restricted:        restricted,
		VisiblePartitions: visible,
	}
	if resp.Nodes == nil {
		resp.Nodes = []monofsstore.DuckDBSQLNodeResult{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

func (s *Service) duckDBScope(ctx context.Context, dataset string) ([]string, bool) {
	if s.policyGate == nil {
		return nil, false
	}
	signal := policy.SignalLogs
	switch dataset {
	case "traces":
		signal = policy.SignalTraces
	case "metrics":
		signal = policy.SignalMetrics
	}
	identity, _ := authz.IdentityFromContext(ctx)
	return s.policyGate.VisiblePartitions(identity, signal)
}

func duckDBSQLErrorStatus(err error) int {
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"forbidden", "only select", "single statement", "may not", "placeholders",
		"unsupported dataset", "query is empty", "query exceeds", "time range",
		"must be before", "narrow the time range",
	} {
		if strings.Contains(message, marker) {
			return http.StatusBadRequest
		}
	}
	return http.StatusBadGateway
}

func parseSQLTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
