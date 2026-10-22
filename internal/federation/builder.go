package federation

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PartitionInfo is the subset of Guardian partition metadata needed to build a
// federation catalog. It is intentionally decoupled from the Guardian API.
type PartitionInfo struct {
	Name     string            `json:"name" yaml:"name"`
	Provider string            `json:"provider,omitempty" yaml:"provider,omitempty"`
	Account  string            `json:"account,omitempty" yaml:"account,omitempty"`
	Region   string            `json:"region,omitempty" yaml:"region,omitempty"`
	Stack    string            `json:"stack,omitempty" yaml:"stack,omitempty"`
	LogGroup string            `json:"log_group,omitempty" yaml:"log_group,omitempty"`
	Labels   map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

// PusherInfo is the subset of the Guardian pusher registry needed to address an
// account/region read API.
type PusherInfo struct {
	Name     string `json:"name" yaml:"name"`
	Account  string `json:"account" yaml:"account"`
	Region   string `json:"region" yaml:"region"`
	Endpoint string `json:"endpoint" yaml:"endpoint"`
	TokenRef string `json:"token_ref,omitempty" yaml:"token_ref,omitempty"`
	TokenEnv string `json:"token_env,omitempty" yaml:"token_env,omitempty"`
	Version  string `json:"version,omitempty" yaml:"version,omitempty"`
	Priority int    `json:"priority,omitempty" yaml:"priority,omitempty"`
	Health   Health `json:"health,omitempty" yaml:"health,omitempty"`
}

// TargetMeta is the discovery payload fetched from a pusher's /v1/aws/meta.
type TargetMeta struct {
	MetricNames  []string `json:"metricNames" yaml:"metricNames"`
	LogGroups    []string `json:"logGroups" yaml:"logGroups"`
	XrayServices []string `json:"xrayServices" yaml:"xrayServices"`
}

// BuildInput is the normalized input for a catalog build.
type BuildInput struct {
	Version    string                `json:"version,omitempty" yaml:"version,omitempty"`
	Partitions []PartitionInfo       `json:"partitions" yaml:"partitions"`
	Pushers    []PusherInfo          `json:"pushers" yaml:"pushers"`
	Meta       map[string]TargetMeta `json:"meta,omitempty" yaml:"meta,omitempty"`
}

// BuildResult contains the snapshot and non-fatal build warnings.
type BuildResult struct {
	Snapshot Snapshot `json:"snapshot" yaml:"snapshot"`
	Warnings []string `json:"warnings,omitempty" yaml:"warnings,omitempty"`
}

type accountRegion struct {
	account string
	region  string
}

// BuildCatalog joins Guardian partition ownership with pusher addressing and
// pusher discovery metadata. The resulting target is per account/region; each
// target carries every partition served by that pusher.
func BuildCatalog(input BuildInput) (BuildResult, error) {
	if len(input.Partitions) == 0 {
		return BuildResult{}, fmt.Errorf("catalog build requires at least one partition")
	}

	pushers := make(map[accountRegion][]PusherInfo)
	for _, pusher := range input.Pushers {
		pusher.Account = strings.TrimSpace(pusher.Account)
		pusher.Region = strings.TrimSpace(pusher.Region)
		if pusher.Account == "" || pusher.Region == "" || strings.TrimSpace(pusher.Endpoint) == "" {
			continue
		}
		pushers[accountRegion{account: pusher.Account, region: pusher.Region}] = append(
			pushers[accountRegion{account: pusher.Account, region: pusher.Region}], pusher)
	}
	for key := range pushers {
		sort.Slice(pushers[key], func(i, j int) bool {
			return betterPusher(pushers[key][i], pushers[key][j])
		})
	}

	groups := make(map[accountRegion][]PartitionInfo)
	warnings := make([]string, 0)
	for _, partition := range input.Partitions {
		partition.Name = strings.TrimSpace(partition.Name)
		partition.Account = strings.TrimSpace(partition.Account)
		partition.Region = strings.TrimSpace(partition.Region)
		provider := strings.ToLower(strings.TrimSpace(partition.Provider))
		if provider == "" && partition.Account != "" && partition.Region != "" {
			provider = "aws"
		}
		if provider != "aws" {
			warnings = append(warnings, fmt.Sprintf("partition %s skipped: provider=%s", partition.Name, provider))
			continue
		}
		if partition.Name == "" || partition.Account == "" || partition.Region == "" {
			warnings = append(warnings, fmt.Sprintf("partition %q skipped: name, account and region are required", partition.Name))
			continue
		}
		key := accountRegion{account: partition.Account, region: partition.Region}
		groups[key] = append(groups[key], partition)
	}

	keys := make([]accountRegion, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].account == keys[j].account {
			return keys[i].region < keys[j].region
		}
		return keys[i].account < keys[j].account
	})

	targets := make([]Target, 0, len(keys))
	for _, key := range keys {
		candidates := pushers[key]
		if len(candidates) == 0 {
			warnings = append(warnings, fmt.Sprintf("account %s/%s skipped: no healthy pusher", key.account, key.region))
			continue
		}
		pusher := candidates[0]
		partitions := groups[key]
		sort.Slice(partitions, func(i, j int) bool { return partitions[i].Name < partitions[j].Name })

		target := Target{
			ID:           "aws-" + key.account + "-" + key.region,
			Account:      key.account,
			Region:       key.region,
			Tenant:       "default",
			Endpoint:     strings.TrimRight(strings.TrimSpace(pusher.Endpoint), "/"),
			TokenRef:     strings.TrimSpace(pusher.TokenRef),
			TokenEnv:     strings.TrimSpace(pusher.TokenEnv),
			Provider:     "aws",
			Capabilities: []Signal{SignalMetrics, SignalLogs, SignalTraces},
			Health:       pusher.Health,
			Version:      pusher.Version,
			Priority:     pusher.Priority,
			Labels:       make(map[string]string),
		}

		for _, partition := range partitions {
			target.Partitions = append(target.Partitions, partition.Name)
			mergeLabels(target.Labels, partition.Labels, &warnings)
			if partition.LogGroup != "" {
				target.LogGroups = append(target.LogGroups, partition.LogGroup)
			}
			// Guardian convention: the CDK app writes to
			// guardian/<account>/<region>/<partition>/app.
			target.LogGroups = append(target.LogGroups,
				fmt.Sprintf("guardian/%s/%s/%s/app", key.account, key.region, partition.Name))
		}

		// Discovery metadata is keyed either by account/region or pusher name.
		meta := input.Meta[key.account+"/"+key.region]
		if len(meta.MetricNames) == 0 && len(meta.LogGroups) == 0 && len(meta.XrayServices) == 0 {
			meta = input.Meta[pusher.Name]
		}
		target.MetricNames = append(target.MetricNames, meta.MetricNames...)
		target.LogGroups = append(target.LogGroups, meta.LogGroups...)
		target.XrayServices = append(target.XrayServices, meta.XrayServices...)

		target.Partitions = dedupeStrings(target.Partitions)
		target.MetricNames = dedupeStrings(target.MetricNames)
		target.LogGroups = dedupeStrings(target.LogGroups)
		target.XrayServices = dedupeStrings(target.XrayServices)
		targets = append(targets, target)
	}

	if len(targets) == 0 {
		return BuildResult{}, fmt.Errorf("catalog build produced no targets")
	}
	version := strings.TrimSpace(input.Version)
	if version == "" {
		version = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	return BuildResult{
		Snapshot: Snapshot{Version: version, GeneratedAt: time.Now().UTC(), Targets: targets},
		Warnings: warnings,
	}, nil
}

func betterPusher(left, right PusherInfo) bool {
	leftScore := pusherHealthScore(left.Health.State)
	rightScore := pusherHealthScore(right.Health.State)
	if leftScore != rightScore {
		return leftScore < rightScore
	}
	if left.Priority != right.Priority {
		return left.Priority < right.Priority
	}
	return left.Name < right.Name
}

func pusherHealthScore(state string) int {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "healthy", "ready":
		return 0
	case "degraded":
		return 1
	case "offline", "down", "failed":
		return 2
	default:
		return 1
	}
}

func mergeLabels(dst map[string]string, src map[string]string, warnings *[]string) {
	for key, value := range src {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		if existing, ok := dst[key]; ok && existing != value {
			*warnings = append(*warnings, fmt.Sprintf("label %s conflict: %q vs %q", key, existing, value))
			continue
		}
		dst[key] = value
	}
}
