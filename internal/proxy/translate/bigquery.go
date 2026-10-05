package translate

import (
	"fmt"
	"strings"
	"time"
)

// BQConfig configures BigQuery SQL generation for logs.
type BQConfig struct {
	Project string
	Dataset string
	Table   string
	Schema  map[string]string
	From    time.Time
	To      time.Time
	Limit   int
}

// DefaultBQSchema maps Doctor's canonical log label vocabulary onto BigQuery
// column names.
func DefaultBQSchema() map[string]string {
	return map[string]string{
		"timestamp": "timestamp",
		"body":      "body",
		"service":   "service",
		"level":     "severity",
		"trace_id":  "trace_id",
		"span_id":   "span_id",
	}
}

// LogQLToBigQuery translates a reduced LogQL query into BigQuery SQL. The
// result is read-only and selects all columns so Doctor can apply parser
// stages, label filters, and metric aggregation locally.
func LogQLToBigQuery(query LogQuery, cfg BQConfig) (string, error) {
	table := strings.TrimSpace(cfg.Table)
	if table == "" {
		return "", fmt.Errorf("%w: bigquery backend requires a table", ErrUnsupported)
	}
	if cfg.Dataset != "" {
		table = cfg.Dataset + "." + table
	}
	if cfg.Project != "" {
		table = cfg.Project + "." + table
	}
	schema := DefaultBQSchema()
	for key, value := range cfg.Schema {
		if strings.TrimSpace(value) != "" {
			schema[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}

	matchers, err := ParseSelector(query.Selector)
	if err != nil {
		return "", err
	}

	conditions := make([]string, 0, len(matchers)+len(query.LineFilters)+2)
	for _, matcher := range matchers {
		field := bqField(schema, matcher.Name)
		switch matcher.Op {
		case "=":
			conditions = append(conditions, field+" = "+bqString(matcher.Value))
		case "!=":
			conditions = append(conditions, field+" != "+bqString(matcher.Value))
		case "=~":
			conditions = append(conditions, "REGEXP_CONTAINS("+field+", "+bqRegex(matcher.Value)+")")
		case "!~":
			conditions = append(conditions, "NOT REGEXP_CONTAINS("+field+", "+bqRegex(matcher.Value)+")")
		default:
			return "", fmt.Errorf("%w: unsupported matcher operator %q", ErrUnsupported, matcher.Op)
		}
	}

	body := bqField(schema, "body")
	for _, filter := range query.LineFilters {
		switch filter.Op {
		case "|=":
			conditions = append(conditions, "STRPOS("+body+", "+bqString(filter.Value)+") > 0")
		case "!=":
			conditions = append(conditions, "STRPOS("+body+", "+bqString(filter.Value)+") = 0")
		case "|~":
			conditions = append(conditions, "REGEXP_CONTAINS("+body+", "+bqRegex(filter.Value)+")")
		case "!~":
			conditions = append(conditions, "NOT REGEXP_CONTAINS("+body+", "+bqRegex(filter.Value)+")")
		default:
			return "", fmt.Errorf("%w: unsupported line filter %q", ErrUnsupported, filter.Op)
		}
	}

	timestamp := bqField(schema, "timestamp")
	if !cfg.From.IsZero() {
		conditions = append(conditions, timestamp+" >= TIMESTAMP("+bqString(cfg.From.UTC().Format(time.RFC3339Nano))+")")
	}
	if !cfg.To.IsZero() {
		conditions = append(conditions, timestamp+" <= TIMESTAMP("+bqString(cfg.To.UTC().Format(time.RFC3339Nano))+")")
	}

	var builder strings.Builder
	builder.WriteString("SELECT * FROM `")
	builder.WriteString(table)
	builder.WriteString("`")
	if len(conditions) > 0 {
		builder.WriteString(" WHERE ")
		builder.WriteString(strings.Join(conditions, " AND "))
	}
	builder.WriteString(" ORDER BY ")
	builder.WriteString(timestamp)
	builder.WriteString(" DESC")
	if cfg.Limit > 0 {
		builder.WriteString(" LIMIT ")
		builder.WriteString(fmt.Sprintf("%d", cfg.Limit))
	}
	return builder.String(), nil
}

func bqField(schema map[string]string, name string) string {
	if mapped, ok := schema[strings.ToLower(name)]; ok && mapped != "" {
		return mapped
	}
	return name
}

func bqString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func bqRegex(pattern string) string {
	escaped := strings.ReplaceAll(pattern, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `r"` + escaped + `"`
}
