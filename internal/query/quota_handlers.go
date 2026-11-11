package query

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/quota"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

type quotaDashboardResponse struct {
	Enabled       bool                  `json:"enabled"`
	PolicyReady   bool                  `json:"policy_ready"`
	PolicyVersion string                `json:"policy_version,omitempty"`
	Mode          string                `json:"mode"`
	Defaults      policy.Quota          `json:"defaults"`
	Subject       []subjectQuotaView    `json:"subject,omitempty"`
	Partitions    []quotaPartitionView  `json:"partitions"`
	Ingest        *ingestQuotaUsageView `json:"ingest,omitempty"`
	Truncated     bool                  `json:"truncated,omitempty"`
	GeneratedAt   string                `json:"generated_at"`
}

type subjectQuotaView struct {
	Key   string        `json:"key"`
	Quota policy.Quota  `json:"quota"`
	Usage []quota.Usage `json:"usage,omitempty"`
}

type quotaPartitionView struct {
	Name        string        `json:"name"`
	Profile     string        `json:"profile,omitempty"`
	Enforcement string        `json:"enforcement"`
	Blocked     bool          `json:"blocked,omitempty"`
	Quota       policy.Quota  `json:"quota"`
	Usage       []quota.Usage `json:"usage,omitempty"`
}

type ingestQuotaUsageView struct {
	Configured  bool          `json:"configured"`
	Enabled     bool          `json:"enabled,omitempty"`
	PolicyReady bool          `json:"policy_ready,omitempty"`
	Usage       []quota.Usage `json:"usage,omitempty"`
	Error       string        `json:"error,omitempty"`
}

func (s *Service) handleQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	resp := quotaDashboardResponse{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	if s.quotaProvider == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Enabled = true
	resp.PolicyReady = s.quotaProvider.Ready()
	if pol := s.quotaProvider.Policy(); pol != nil {
		resp.PolicyVersion = pol.Version()
	}
	resp.Defaults = s.quotaProvider.EffectiveDefaults()

	identity, _ := authz.IdentityFromContext(r.Context())
	var allUsage []quota.Usage
	if s.quotaEngine != nil {
		allUsage = s.quotaEngine.Snapshot("")
	}
	for _, scoped := range s.quotaProvider.SubjectQuotas(identity) {
		view := subjectQuotaView{Key: scoped.Key, Quota: scoped.Quota}
		view.Usage = filterUsage(allUsage, scoped.Key+":")
		resp.Subject = append(resp.Subject, view)
	}
	resp.Mode = "observe"
	if s.quotaProvider.Enforce("") {
		resp.Mode = "enforce"
	}

	partitions := s.quotaPartitions(identity)
	if filter := strings.TrimSpace(r.URL.Query().Get("partition")); filter != "" {
		filtered := make([]string, 0, 1)
		for _, partition := range partitions {
			if partition == filter {
				filtered = append(filtered, partition)
			}
		}
		partitions = filtered
	}
	if limit := parseQuotaLimit(r.URL.Query().Get("limit")); limit > 0 && len(partitions) > limit {
		partitions = partitions[:limit]
		resp.Truncated = true
	}
	resp.Partitions = make([]quotaPartitionView, 0, len(partitions))
	for _, partition := range partitions {
		q := s.quotaProvider.PartitionQuota(partition)
		view := quotaPartitionView{
			Name:        partition,
			Profile:     q.Profile,
			Enforcement: enforcementLabel(s.quotaProvider.Enforce(partition)),
			Blocked:     q.Blocked,
			Quota:       q,
		}
		if s.quotaEngine != nil {
			view.Usage = filterUsage(allUsage, "partition:"+partition+":")
		}
		resp.Partitions = append(resp.Partitions, view)
	}
	resp.Ingest = s.ingestQuotaUsage(r.Context())
	if resp.Ingest != nil && len(resp.Ingest.Usage) > 0 {
		allowed := make(map[string]struct{}, len(resp.Partitions))
		for _, partition := range resp.Partitions {
			allowed["partition:"+partition.Name+":"] = struct{}{}
		}
		filtered := make([]quota.Usage, 0, len(resp.Ingest.Usage))
		for _, usage := range resp.Ingest.Usage {
			for prefix := range allowed {
				if strings.HasPrefix(usage.Key, prefix) {
					filtered = append(filtered, usage)
					break
				}
			}
		}
		resp.Ingest.Usage = filtered
	}
	writeJSON(w, http.StatusOK, resp)
}

func enforcementLabel(enforce bool) string {
	if enforce {
		return "enforce"
	}
	return "observe"
}

func filterUsage(all []quota.Usage, prefix string) []quota.Usage {
	if len(all) == 0 {
		return nil
	}
	out := make([]quota.Usage, 0, 2)
	for _, usage := range all {
		if strings.HasPrefix(usage.Key, prefix) {
			out = append(out, usage)
		}
	}
	return out
}

func parseQuotaLimit(raw string) int {
	const defaultLimit = 500
	const maxLimit = 5000
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultLimit
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return defaultLimit
	}
	if value > maxLimit {
		return maxLimit
	}
	return value
}

// enforceSeriesLimit rejects a response whose result cardinality exceeds the
// partition/user maxSeries budget. It is called by the main query handlers.
func (s *Service) enforceSeriesLimit(w http.ResponseWriter, r *http.Request, data any) bool {
	if s == nil || s.quotaProvider == nil {
		return true
	}
	identity, _ := authz.IdentityFromContext(r.Context())
	partitions := quota.PartitionsFromRequest(r)
	limit := s.quotaProvider.QueryLimits(identity, partitions).MaxSeries
	if limit <= 0 {
		return true
	}
	enforce := s.quotaProvider.Enforce("")
	if len(partitions) > 0 {
		enforce = false
		for _, partition := range partitions {
			if s.quotaProvider.Enforce(partition) {
				enforce = true
				break
			}
		}
	}
	for _, scoped := range s.quotaProvider.SubjectQuotas(identity) {
		if strings.EqualFold(scoped.Quota.Enforcement, "enforce") {
			enforce = true
			break
		}
	}
	if !enforce {
		return true
	}
	count := countResultSeries(data)
	if count <= int(limit) {
		return true
	}
	quota.WriteLimitError(w, &quota.LimitError{
		Key:      "query:series",
		Resource: "query.series",
		Limit:    float64(limit),
	})
	return false
}

func countResultSeries(data any) int {
	switch value := data.(type) {
	case nil:
		return 0
	case []any:
		return len(value)
	case map[string]any:
		for _, key := range []string{"result", "traces", "series"} {
			if count, ok := sliceLen(value[key]); ok {
				return count
			}
		}
		if nested, ok := value["data"].(map[string]any); ok {
			return countResultSeries(nested)
		}
		if count, ok := sliceLen(value["data"]); ok {
			return count
		}
	}
	if count, ok := sliceLen(data); ok {
		return count
	}
	return 0
}

func sliceLen(value any) (int, bool) {
	if value == nil {
		return 0, false
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array:
		return reflected.Len(), true
	default:
		return 0, false
	}
}

// quotaPartitions returns the partitions this caller may see. Metrics are
// globally readable, so an unrestricted caller receives the whole catalog.
func (s *Service) quotaPartitions(identity authz.Identity) []string {
	if s.policyGate != nil {
		if visible, restricted := s.policyGate.AccessiblePartitions(identity); restricted {
			return visible
		}
	}
	known := s.quotaProvider.KnownPartitions()
	if len(known) == 0 {
		if partition := strings.TrimSpace(s.defaultPartition); partition != "" {
			return []string{partition}
		}
		return nil
	}
	out := append([]string(nil), known...)
	sort.Strings(out)
	return out
}

func (s *Service) ingestQuotaUsage(ctx context.Context) *ingestQuotaUsageView {
	if s.ingestURL == "" {
		return &ingestQuotaUsageView{Configured: false}
	}
	view := &ingestQuotaUsageView{Configured: true}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ingestURL+"/v1/quota/usage", nil)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if s.ingestToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.ingestToken)
	}
	resp, err := s.ingestClient.Do(req)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		view.Error = resp.Status
		return view
	}
	var payload struct {
		Enabled     bool          `json:"enabled"`
		PolicyReady bool          `json:"policy_ready"`
		Usage       []quota.Usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		view.Error = err.Error()
		return view
	}
	view.Enabled = payload.Enabled
	view.PolicyReady = payload.PolicyReady
	view.Usage = payload.Usage
	return view
}
