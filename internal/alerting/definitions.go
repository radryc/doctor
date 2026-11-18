package alerting

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AlertInstanceCounts summarizes live alert instances for one definition.
type AlertInstanceCounts struct {
	Firing   int `json:"firing"`
	Pending  int `json:"pending"`
	Silenced int `json:"silenced"`
	Resolved int `json:"resolved"`
	NoData   int `json:"no_data"`
	Error    int `json:"error"`
	Total    int `json:"total"`
}

func (c *AlertInstanceCounts) add(status string) {
	switch status {
	case StatusFiring:
		c.Firing++
	case StatusPending:
		c.Pending++
	case StatusSilenced:
		c.Silenced++
	case StatusResolved:
		c.Resolved++
	case StatusNoData:
		c.NoData++
	case StatusError:
		c.Error++
	}
	c.Total++
}

// AlertDefinition is one alert that can fire for a partition. Defined alerts
// come from Sentinel PromQL/guided rules, Guardian's built-in partition
// alerts, or the AWS/GCP source policies. Unmanaged source alerts are included
// so alerts pushed by Guardian or the test intake are never invisible.
type AlertDefinition struct {
	Name        string              `json:"name"`
	Engine      string              `json:"engine"` // promql | guided | guardian | aws | gcp | source
	Source      string              `json:"source"` // sentinel | guardian | aws | gcp | test
	Group       string              `json:"group,omitempty"`
	Mode        string              `json:"mode,omitempty"`
	Expr        string              `json:"expr,omitempty"`
	For         string              `json:"for,omitempty"`
	Severity    string              `json:"severity,omitempty"`
	Summary     string              `json:"summary,omitempty"`
	Notify      []string            `json:"notify,omitempty"`
	Labels      map[string]string   `json:"labels,omitempty"`
	Annotations map[string]string   `json:"annotations,omitempty"`
	Spec        *AlertSpec          `json:"spec,omitempty"`
	Defined     bool                `json:"defined"`
	Instances   AlertInstanceCounts `json:"instances"`
}

func (a *API) handleInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := a.identity(r)
	if tenant == "" {
		if a.Service != nil && strings.TrimSpace(a.Service.DefaultTenant) != "" {
			tenant = a.Service.DefaultTenant
		} else {
			tenant = "default"
		}
	}
	if partition == "" {
		writeError(w, http.StatusBadRequest, "partition is required")
		return
	}
	if !identity.Can(CapAlertsRead, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	definitions, err := a.listAlertDefinitions(r.Context(), tenant, partition)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":      tenant,
		"partition":   partition,
		"definitions": definitions,
	})
}

// listAlertDefinitions builds the per-partition alert catalog: rule-based
// definitions, built-in source alerts, and unmanaged source instances.
func (a *API) listAlertDefinitions(ctx context.Context, tenant, partition string) ([]AlertDefinition, error) {
	defs := make([]AlertDefinition, 0)
	index := make(map[string]int) // source + "\x00" + name

	add := func(def AlertDefinition) {
		key := def.Source + "\x00" + def.Name
		if pos, ok := index[key]; ok {
			// Merge instance counts if the same definition is contributed twice.
			defs[pos].Instances.Firing += def.Instances.Firing
			defs[pos].Instances.Pending += def.Instances.Pending
			defs[pos].Instances.Silenced += def.Instances.Silenced
			defs[pos].Instances.Resolved += def.Instances.Resolved
			defs[pos].Instances.NoData += def.Instances.NoData
			defs[pos].Instances.Error += def.Instances.Error
			defs[pos].Instances.Total += def.Instances.Total
			return
		}
		index[key] = len(defs)
		defs = append(defs, def)
	}

	// 1. Sentinel rules: PromQL and guided.
	refs, err := a.Store.ListRuleGroups(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("list alert rule groups: %w", err)
	}
	for _, ref := range refs {
		if ref.Partition != partition || ref.File == nil {
			continue
		}
		for _, group := range ref.File.Groups {
			for _, rule := range group.Rules {
				mode := rule.ModeOrDefault()
				severity := rule.Labels["severity"]
				if severity == "" {
					severity = "warning"
				}
				add(AlertDefinition{
					Name:        rule.Alert,
					Engine:      mode,
					Source:      "sentinel",
					Group:       group.Name,
					Mode:        mode,
					Expr:        rule.Expr,
					For:         formatDuration(time.Duration(rule.For)),
					Severity:    severity,
					Summary:     rule.Annotations["summary"],
					Notify:      append([]string(nil), rule.Notify...),
					Labels:      cloneStringMap(rule.Labels),
					Annotations: cloneStringMap(rule.Annotations),
					Spec:        rule.Spec,
					Defined:     true,
				})
			}
		}
	}

	// 2. Guardian built-in partition alerts.
	guardianPolicy, err := a.Store.GetGuardianPolicy(ctx, tenant, partition)
	if err != nil {
		return nil, fmt.Errorf("get guardian policy: %w", err)
	}
	for _, builtin := range []struct {
		name     string
		severity string
		summary  string
	}{
		{"GuardianPartitionError", "critical", "Guardian partition is failing or unhealthy."},
		{"GuardianPartitionAttention", "warning", "Guardian partition needs attention (drift or degraded health)."},
		{"GuardianPartitionUnavailable", "warning", "Guardian could not observe the partition (status read timed out or returned unavailable)."},
	} {
		add(AlertDefinition{
			Name:     builtin.name,
			Engine:   "guardian",
			Source:   "guardian",
			For:      formatDuration(guardianPolicy.For(builtin.name)),
			Severity: builtin.severity,
			Summary:  builtin.summary,
			Defined:  true,
		})
	}

	// 3. AWS/GCP source policies. These are pull sources: the policy selects
	// which discovered alarms are imported, and the instances carry source=aws
	// or source=gcp.
	awsPolicy, err := a.Store.GetAWSPolicy(ctx)
	if err != nil {
		return nil, fmt.Errorf("get aws policy: %w", err)
	}
	add(AlertDefinition{
		Name:     "aws-cloudwatch",
		Engine:   "aws",
		Source:   "aws",
		Mode:     "pull",
		Severity: "warning",
		Summary:  fmt.Sprintf("CloudWatch alarm selection: %s (include=%d exclude=%d)", awsPolicy.Default, len(awsPolicy.Include), len(awsPolicy.Exclude)),
		Defined:  true,
	})
	gcpPolicy, err := a.Store.GetGCPPolicy(ctx)
	if err != nil {
		return nil, fmt.Errorf("get gcp policy: %w", err)
	}
	add(AlertDefinition{
		Name:     "gcp-monitoring",
		Engine:   "gcp",
		Source:   "gcp",
		Mode:     "pull",
		Severity: "warning",
		Summary:  fmt.Sprintf("Cloud Monitoring alert selection: %s (include=%d exclude=%d)", gcpPolicy.Default, len(gcpPolicy.Include), len(gcpPolicy.Exclude)),
		Defined:  true,
	})

	// 4. Live instances, including unmanaged source alerts.
	alerts, err := a.Store.ListAlerts(ctx, tenant, partition)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	a.decorateAlertStatuses(ctx, alerts)

	for _, alert := range alerts {
		name := strings.TrimSpace(alert.Labels["alertname"])
		if name == "" {
			name = alert.Fingerprint
			if len(name) > 12 {
				name = name[:12]
			}
		}
		matched := false
		// Rule and Guardian definitions match by alert name.
		for i := range defs {
			if defs[i].Source == "aws" || defs[i].Source == "gcp" {
				continue
			}
			if defs[i].Name == name {
				defs[i].Instances.add(alert.Status)
				matched = true
				break
			}
		}
		if !matched {
			// Source-policy definitions aggregate all alerts from their source.
			for i := range defs {
				if defs[i].Source == alert.Source && (defs[i].Engine == "aws" || defs[i].Engine == "gcp") {
					defs[i].Instances.add(alert.Status)
					matched = true
					break
				}
			}
		}
		if matched {
			continue
		}
		// Unmanaged: an alert instance with no rule or built-in definition
		// (for example source=test or a manually pushed Guardian alert).
		summary := alert.Annotations["summary"]
		if summary == "" {
			summary = "Source alert with no rule definition."
		}
		add(AlertDefinition{
			Name:        name,
			Engine:      "source",
			Source:      alert.Source,
			Severity:    alert.Severity,
			Summary:     summary,
			Annotations: cloneStringMap(alert.Annotations),
			Labels:      cloneStringMap(alert.Labels),
			Defined:     false,
			Instances: AlertInstanceCounts{
				Firing:   boolInt(alert.Status == StatusFiring),
				Pending:  boolInt(alert.Status == StatusPending),
				Silenced: boolInt(alert.Status == StatusSilenced),
				Resolved: boolInt(alert.Status == StatusResolved),
				NoData:   boolInt(alert.Status == StatusNoData),
				Error:    boolInt(alert.Status == StatusError),
				Total:    1,
			},
		})
	}

	sort.SliceStable(defs, func(i, j int) bool {
		if defs[i].Defined != defs[j].Defined {
			return defs[i].Defined
		}
		if defs[i].Engine != defs[j].Engine {
			return defs[i].Engine < defs[j].Engine
		}
		if defs[i].Name != defs[j].Name {
			return defs[i].Name < defs[j].Name
		}
		return defs[i].Source < defs[j].Source
	})
	return defs, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
