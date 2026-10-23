package federation

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"sigs.k8s.io/yaml"
)

// Catalog is an in-memory, read-optimized projection of the federation
// targets. It is safe for concurrent reads.
type Catalog struct {
	mu sync.RWMutex

	version    string
	targets    map[string]Target
	targetList []Target

	byAccount   map[string][]string
	byRegion    map[string][]string
	byPartition map[string][]string
	byLogGroup  map[string][]string
	byService   map[string][]string
	byMetric    map[string][]string
	byTrace     map[string]string
}

// LoadCatalogFile loads a static YAML or JSON catalog. The file may be either
// a Snapshot document (`targets: [...]`) or a bare list of targets.
//
// Targets may specify `token_env`; the token is resolved from the process
// environment at load time. Tokens are never stored in the snapshot.
func LoadCatalogFile(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read federation catalog %s: %w", path, err)
	}
	return LoadCatalog(data)
}

// LoadSnapshotFile parses a catalog snapshot from a local file.
func LoadSnapshotFile(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read federation catalog %s: %w", path, err)
	}
	return ParseSnapshot(data)
}

// ParseSnapshot parses a catalog document. The file may be either a Snapshot
// document (`targets: [...]`) or a bare list of targets.
func ParseSnapshot(data []byte) (Snapshot, error) {
	var snapshot Snapshot
	snapErr := yaml.Unmarshal(data, &snapshot)
	if snapErr == nil && len(snapshot.Targets) > 0 {
		return snapshot, nil
	}

	var targets []Target
	if err := yaml.Unmarshal(data, &targets); err != nil {
		if snapErr != nil {
			return Snapshot{}, fmt.Errorf("parse federation catalog: snapshot: %v; list: %w", snapErr, err)
		}
		return Snapshot{}, fmt.Errorf("parse federation catalog: %w", err)
	}
	if len(targets) == 0 {
		return Snapshot{}, fmt.Errorf("federation catalog contains no targets")
	}
	return Snapshot{Version: snapshot.Version, Targets: targets}, nil
}

// LoadCatalog parses a catalog document from bytes.
func LoadCatalog(data []byte) (*Catalog, error) {
	snapshot, err := ParseSnapshot(data)
	if err != nil {
		return nil, err
	}
	return NewCatalog(snapshot.Targets, snapshot.Version)
}

// NewCatalog builds indexes from targets.
func NewCatalog(targets []Target, version string) (*Catalog, error) {
	c := &Catalog{
		version:     strings.TrimSpace(version),
		targets:     make(map[string]Target, len(targets)),
		byAccount:   make(map[string][]string),
		byRegion:    make(map[string][]string),
		byPartition: make(map[string][]string),
		byLogGroup:  make(map[string][]string),
		byService:   make(map[string][]string),
		byMetric:    make(map[string][]string),
		byTrace:     make(map[string]string),
	}

	for _, t := range targets {
		t = normalizeTarget(t)
		if err := validateTarget(t); err != nil {
			return nil, err
		}
		if _, exists := c.targets[t.ID]; exists {
			return nil, fmt.Errorf("duplicate federation target id %q", t.ID)
		}
		c.targets[t.ID] = t

		// Target fields are already deduplicated by normalizeTarget, so a
		// plain append is sufficient. Using an append-and-scan-until-unique
		// helper here is O(n^2) for hot keys such as one region or one metric
		// shared by every target.
		c.byAccount[t.Account] = append(c.byAccount[t.Account], t.ID)
		c.byRegion[t.Region] = append(c.byRegion[t.Region], t.ID)
		for _, partition := range t.Partitions {
			c.byPartition[partition] = append(c.byPartition[partition], t.ID)
		}
		for _, group := range t.LogGroups {
			c.byLogGroup[group] = append(c.byLogGroup[group], t.ID)
		}
		for _, service := range t.XrayServices {
			c.byService[service] = append(c.byService[service], t.ID)
		}
		for _, metric := range t.MetricNames {
			c.byMetric[metric] = append(c.byMetric[metric], t.ID)
		}
	}

	normalizeIndex(c.byAccount)
	normalizeIndex(c.byRegion)
	normalizeIndex(c.byPartition)
	normalizeIndex(c.byLogGroup)
	normalizeIndex(c.byService)
	normalizeIndex(c.byMetric)

	c.targetList = make([]Target, 0, len(c.targets))
	for _, target := range c.targets {
		c.targetList = append(c.targetList, target)
	}
	// Preserve the historical Targets() ordering (ID ascending). Planner
	// results are re-sorted by priority separately.
	sort.Slice(c.targetList, func(i, j int) bool { return c.targetList[i].ID < c.targetList[j].ID })
	return c, nil
}

// normalizeIndex sorts each posting list and removes duplicate target IDs.
// Posting lists are built once per catalog, so this stays O(n log n) overall
// instead of the previous O(n^2) uniqueness scan on every append.
func normalizeIndex(index map[string][]string) {
	for key, values := range index {
		if len(values) == 0 {
			delete(index, key)
			continue
		}
		sort.Strings(values)
		out := values[:0]
		for _, value := range values {
			if len(out) == 0 || out[len(out)-1] != value {
				out = append(out, value)
			}
		}
		index[key] = out
	}
}

func normalizeTarget(t Target) Target {
	t.ID = strings.TrimSpace(t.ID)
	t.Account = strings.TrimSpace(t.Account)
	t.Region = strings.TrimSpace(t.Region)
	t.Endpoint = strings.TrimRight(strings.TrimSpace(t.Endpoint), "/")
	t.Tenant = strings.TrimSpace(t.Tenant)
	t.Provider = strings.TrimSpace(t.Provider)
	if t.Tenant == "" {
		t.Tenant = "default"
	}
	if t.Provider == "" {
		t.Provider = "aws"
	}
	if t.ID == "" && t.Account != "" && t.Region != "" {
		t.ID = "aws-" + t.Account + "-" + t.Region
	}
	if t.Token == "" && t.TokenEnv != "" {
		t.Token = strings.TrimSpace(os.Getenv(t.TokenEnv))
	}
	t.Partitions = dedupeStrings(t.Partitions)
	t.MetricNames = dedupeStrings(t.MetricNames)
	t.LogGroups = dedupeStrings(t.LogGroups)
	t.XrayServices = dedupeStrings(t.XrayServices)
	return t
}

func validateTarget(t Target) error {
	if t.ID == "" {
		return invalidTarget(t.ID, "id")
	}
	if t.Account == "" {
		return invalidTarget(t.ID, "account")
	}
	if t.Region == "" {
		return invalidTarget(t.ID, "region")
	}
	if t.Endpoint == "" {
		return invalidTarget(t.ID, "endpoint")
	}
	if t.Token == "" && t.TokenRef == "" {
		if t.TokenEnv != "" {
			return fmt.Errorf("federation target %q: token_env %s is empty", t.ID, t.TokenEnv)
		}
		return fmt.Errorf("federation target %q: token, token_env, or token_ref is required", t.ID)
	}
	return nil
}

// Len returns the number of targets.
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.targets)
}

// Version returns the catalog version.
func (c *Catalog) Version() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

// Targets returns all targets sorted by ID.
func (c *Catalog) Targets() []Target {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	// targetList is sorted once at construction time. Return a shallow copy so
	// callers cannot reorder or replace the catalog's slice.
	return append([]Target(nil), c.targetList...)
}

// ByAccounts resolves account IDs.
func (c *Catalog) ByAccounts(accounts []string) []Target {
	return c.byValues(c.byAccount, accounts)
}

// ByRegions resolves regions.
func (c *Catalog) ByRegions(regions []string) []Target {
	return c.byValues(c.byRegion, regions)
}

// ByPartitions resolves Guardian partitions.
func (c *Catalog) ByPartitions(partitions []string) []Target {
	return c.byValues(c.byPartition, partitions)
}

// ByLogGroups resolves concrete CloudWatch log group names.
func (c *Catalog) ByLogGroups(groups []string) []Target {
	return c.byValues(c.byLogGroup, groups)
}

// ByServices resolves X-Ray service names.
func (c *Catalog) ByServices(services []string) []Target {
	return c.byValues(c.byService, services)
}

// ByMetric resolves CloudWatch metric names. A target with no cached metric
// names does not match; the planner may fall back to all targets when the
// catalog is small.
func (c *Catalog) ByMetric(metric string) []Target {
	if strings.TrimSpace(metric) == "" {
		return nil
	}
	return c.byValues(c.byMetric, []string{metric})
}

// LogGroupsMatching returns cached log groups whose names match pattern.
func (c *Catalog) LogGroupsMatching(pattern string) []string {
	if c == nil || strings.TrimSpace(pattern) == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0)
	for group := range c.byLogGroup {
		if re.MatchString(group) {
			out = append(out, group)
		}
	}
	sort.Strings(out)
	return out
}

// ByTrace resolves a trace ID when a trace index is present.
func (c *Catalog) ByTrace(traceID string) (Target, bool) {
	if c == nil {
		return Target{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.byTrace[strings.TrimSpace(traceID)]
	if !ok {
		return Target{}, false
	}
	t, ok := c.targets[id]
	return t, ok
}

func (c *Catalog) byValues(index map[string][]string, values []string) []Target {
	if c == nil || len(values) == 0 {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := make(map[string]struct{})
	out := make([]Target, 0)
	for _, value := range values {
		for _, id := range index[strings.TrimSpace(value)] {
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			if t, ok := c.targets[id]; ok {
				out = append(out, t)
			}
		}
	}
	sortTargets(out)
	return out
}

func (c *Catalog) byRegex(index map[string][]string, patterns []string) []Target {
	if c == nil || len(patterns) == 0 {
		return nil
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		if re, err := regexp.Compile(pattern); err == nil {
			compiled = append(compiled, re)
		}
	}
	if len(compiled) == 0 {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := make(map[string]struct{})
	out := make([]Target, 0)
	for value, ids := range index {
		for _, re := range compiled {
			if !re.MatchString(value) {
				continue
			}
			for _, id := range ids {
				if _, exists := seen[id]; exists {
					continue
				}
				seen[id] = struct{}{}
				if t, ok := c.targets[id]; ok {
					out = append(out, t)
				}
			}
			break
		}
	}
	sortTargets(out)
	return out
}

func dedupeTargets(targets []Target) []Target {
	if len(targets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(targets))
	out := make([]Target, 0, len(targets))
	for _, target := range targets {
		if _, exists := seen[target.ID]; exists {
			continue
		}
		seen[target.ID] = struct{}{}
		out = append(out, target)
	}
	sortTargets(out)
	return out
}

func sortTargets(targets []Target) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Priority != targets[j].Priority {
			return targets[i].Priority < targets[j].Priority
		}
		return targets[i].ID < targets[j].ID
	})
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
