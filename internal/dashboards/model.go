package dashboards

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Version identifies the dashboard document schema.
const Version = "doctor.dashboard/v1"

// BundleVersion identifies a multi-dashboard bundle.
const BundleVersion = "doctor.dashboard.bundle/v1"

// WidgetKind classifies dashboard widget panel types.
type WidgetKind string

const (
	WidgetTimeseries WidgetKind = "timeseries"
	WidgetStat       WidgetKind = "stat"
	WidgetToplist    WidgetKind = "toplist"
	WidgetGauge      WidgetKind = "gauge"
	WidgetTable      WidgetKind = "table"
	WidgetLogs       WidgetKind = "logs"
	WidgetTraces     WidgetKind = "traces"
	WidgetTopology   WidgetKind = "topology"
	WidgetMarkdown   WidgetKind = "markdown"
)

// Datasource names a query backend.
type Datasource string

const (
	DatasourcePrometheus   Datasource = "prometheus"
	DatasourceLoki         Datasource = "loki"
	DatasourceTempo        Datasource = "tempo"
	DatasourceNativeLogs   Datasource = "native_logs"
	DatasourceNativeTraces Datasource = "native_traces"
)

// VariableType describes how a template variable is resolved.
type VariableType string

const (
	VariableLabelValues VariableType = "label_values"
	VariableCustom      VariableType = "custom"
	VariableText        VariableType = "text"
	VariablePartition   VariableType = "partition"
	VariableService     VariableType = "service"
)

// DashboardVariable is a template variable that can be selected by the user.
type DashboardVariable struct {
	Name         string       `json:"name" yaml:"name"`
	Label        string       `json:"label" yaml:"label"`
	Type         VariableType `json:"type" yaml:"type"`
	LabelKey     string       `json:"labelKey,omitempty" yaml:"labelKey,omitempty"`
	MetricFilter string       `json:"metricFilter,omitempty" yaml:"metricFilter,omitempty"`
	Options      []string     `json:"options,omitempty" yaml:"options,omitempty"`
	DefaultValue string       `json:"defaultValue" yaml:"defaultValue"`
	Multi        bool         `json:"multi,omitempty" yaml:"multi,omitempty"`
	IncludeAll   bool         `json:"includeAll,omitempty" yaml:"includeAll,omitempty"`
}

// GridPos defines a widget's position in the 12-column grid.
type GridPos struct {
	X int `json:"x" yaml:"x"`
	Y int `json:"y" yaml:"y"`
	W int `json:"w" yaml:"w"`
	H int `json:"h" yaml:"h"`
}

// ThresholdLevel is a value/color pair for conditional coloring.
type ThresholdLevel struct {
	Value float64 `json:"value" yaml:"value"`
	Color string  `json:"color" yaml:"color"` // "green", "amber", or "red"
}

// WidgetVisualization controls display settings for a widget.
type WidgetVisualization struct {
	DisplayMode     string           `json:"displayMode,omitempty" yaml:"displayMode,omitempty"` // "line", "area", "bars"
	Stacked         bool             `json:"stacked,omitempty" yaml:"stacked,omitempty"`
	ShowExemplars   bool             `json:"showExemplars,omitempty" yaml:"showExemplars,omitempty"`
	Unit            string           `json:"unit,omitempty" yaml:"unit,omitempty"` // "none", "short", "percent", "seconds", "ms", "bytes", "bps", "reqps"
	Decimals        int              `json:"decimals,omitempty" yaml:"decimals,omitempty"`
	Thresholds      []ThresholdLevel `json:"thresholds,omitempty" yaml:"thresholds,omitempty"`
	MarkdownContent string           `json:"markdownContent,omitempty" yaml:"markdownContent,omitempty"`
}

// WidgetQuery is one data query attached to a widget.
type WidgetQuery struct {
	RefID        string     `json:"refId" yaml:"refId"`
	Datasource   Datasource `json:"datasource" yaml:"datasource"`
	Expr         string     `json:"expr" yaml:"expr"`
	LegendFormat string     `json:"legendFormat,omitempty" yaml:"legendFormat,omitempty"`
	Disabled     bool       `json:"disabled,omitempty" yaml:"disabled,omitempty"`
}

// DashboardWidget is a single panel on a dashboard.
type DashboardWidget struct {
	ID            string               `json:"id" yaml:"id"`
	Title         string               `json:"title" yaml:"title"`
	Description   string               `json:"description,omitempty" yaml:"description,omitempty"`
	Kind          WidgetKind           `json:"kind" yaml:"kind"`
	GridPos       GridPos              `json:"gridPos" yaml:"gridPos"`
	Queries       []WidgetQuery        `json:"queries" yaml:"queries"`
	Visualization *WidgetVisualization `json:"visualization,omitempty" yaml:"visualization,omitempty"`
}

// DashboardMeta holds MonoFS versioning metadata.
type DashboardMeta struct {
	VersionID string `json:"versionId,omitempty" yaml:"versionId,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty" yaml:"updatedBy,omitempty"`
}

// DashboardDocument is the top-level portable dashboard schema.
type DashboardDocument struct {
	Version          string              `json:"version" yaml:"version"`
	ID               string              `json:"id" yaml:"id"`
	Title            string              `json:"title" yaml:"title"`
	Description      string              `json:"description,omitempty" yaml:"description,omitempty"`
	Tenant           string              `json:"tenant" yaml:"tenant"`
	Tags             []string            `json:"tags,omitempty" yaml:"tags,omitempty"`
	BuiltIn          bool                `json:"builtIn,omitempty" yaml:"builtIn,omitempty"`
	DefaultTimeRange string              `json:"defaultTimeRange,omitempty" yaml:"defaultTimeRange,omitempty"`
	RefreshInterval  string              `json:"refreshInterval,omitempty" yaml:"refreshInterval,omitempty"`
	Variables        []DashboardVariable `json:"variables,omitempty" yaml:"variables,omitempty"`
	Layout           []DashboardWidget   `json:"layout,omitempty" yaml:"layout,omitempty"`
	Meta             *DashboardMeta      `json:"meta,omitempty" yaml:"meta,omitempty"`
}

// DashboardBundle is a portable export of multiple dashboards.
type DashboardBundle struct {
	Version    string              `json:"version" yaml:"version"`
	ExportedAt time.Time           `json:"exportedAt" yaml:"exportedAt"`
	Dashboards []DashboardDocument `json:"dashboards" yaml:"dashboards"`
}

// ParseDocument decodes a DashboardDocument from JSON or YAML bytes.
func ParseDocument(data []byte) (*DashboardDocument, error) {
	// yaml.Unmarshal also handles JSON.
	var doc DashboardDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("dashboards: parse: %w", err)
	}
	if doc.Version != Version {
		return nil, fmt.Errorf("dashboards: unsupported version %q (want %q)", doc.Version, Version)
	}
	if strings.TrimSpace(doc.ID) == "" {
		return nil, fmt.Errorf("dashboards: missing id")
	}
	if strings.TrimSpace(doc.Title) == "" {
		return nil, fmt.Errorf("dashboards: missing title")
	}
	return &doc, nil
}

// ParseBundle decodes a DashboardBundle from JSON or YAML bytes.
func ParseBundle(data []byte) (*DashboardBundle, error) {
	var bundle DashboardBundle
	if err := yaml.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("dashboards: parse bundle: %w", err)
	}
	if bundle.Version != BundleVersion {
		return nil, fmt.Errorf("dashboards: unsupported bundle version %q (want %q)", bundle.Version, BundleVersion)
	}
	return &bundle, nil
}

// MarshalJSON returns the document as compact JSON.
func (d *DashboardDocument) MarshalJSON() ([]byte, error) {
	type alias DashboardDocument
	return json.Marshal((*alias)(d))
}

// MarshalYAML returns the document as YAML bytes.
func (d *DashboardDocument) MarshalYAMLBytes() ([]byte, error) {
	return yaml.Marshal(d)
}

// DashboardVersion represents a historical MonoFS revision of a dashboard.
type DashboardVersion struct {
	VersionID       string    `json:"versionId"`
	BatchRevisionID string    `json:"batchRevisionId,omitempty"`
	ContentSHA256   string    `json:"contentSha256,omitempty"`
	CommittedAt     time.Time `json:"committedAt"`
	PrincipalID     string    `json:"principalId,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}
