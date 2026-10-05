package translate

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LogLineFilter is one LogQL line filter.
type LogLineFilter struct {
	Op    string
	Value string
}

// LogQuery is the reduced LogQL query sent to a logs backend.
type LogQuery struct {
	Selector    string
	LineFilters []LogLineFilter
}

// ESConfig configures ES|QL generation.
type ESConfig struct {
	// Index is the ES|QL FROM target, e.g. "logs-*.otel-*".
	Index string
	// Schema overrides the default OTel field mapping.
	Schema map[string]string
	From   time.Time
	To     time.Time
	Limit  int
}

// DefaultESSchema maps Doctor's canonical log label vocabulary onto the OTel
// Elasticsearch data-stream field names.
func DefaultESSchema() map[string]string {
	return map[string]string{
		"timestamp": "@timestamp",
		"body":      "body.text",
		"service":   "resource.attributes.service.name",
		"level":     "severity_text",
		"trace_id":  "trace.id",
		"span_id":   "span.id",
	}
}

// LogQLToESQL translates a reduced LogQL query into an ES|QL statement. Parser
// stages, label filters, and metric aggregation are evaluated locally by
// Doctor, so only the stream selector and line filters are translated.
func LogQLToESQL(query LogQuery, cfg ESConfig) (string, error) {
	index := strings.TrimSpace(cfg.Index)
	if index == "" {
		return "", fmt.Errorf("%w: elasticsearch backend requires an index", ErrUnsupported)
	}
	schema := DefaultESSchema()
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
		field := esField(schema, matcher.Name)
		switch matcher.Op {
		case "=":
			conditions = append(conditions, field+" == "+esString(matcher.Value))
		case "!=":
			conditions = append(conditions, field+" != "+esString(matcher.Value))
		case "=~":
			conditions = append(conditions, field+" RLIKE "+esString(anchorRegex(matcher.Value)))
		case "!~":
			conditions = append(conditions, "NOT ("+field+" RLIKE "+esString(anchorRegex(matcher.Value))+")")
		default:
			return "", fmt.Errorf("%w: unsupported matcher operator %q", ErrUnsupported, matcher.Op)
		}
	}

	body := esField(schema, "body")
	for _, filter := range query.LineFilters {
		switch filter.Op {
		case "|=":
			conditions = append(conditions, body+" LIKE "+esString(likeContains(filter.Value)))
		case "!=":
			conditions = append(conditions, "NOT ("+body+" LIKE "+esString(likeContains(filter.Value))+")")
		case "|~":
			conditions = append(conditions, body+" RLIKE "+esString(filter.Value))
		case "!~":
			conditions = append(conditions, "NOT ("+body+" RLIKE "+esString(filter.Value)+")")
		default:
			return "", fmt.Errorf("%w: unsupported line filter %q", ErrUnsupported, filter.Op)
		}
	}

	timestamp := esField(schema, "timestamp")
	if !cfg.From.IsZero() {
		conditions = append(conditions, timestamp+" >= "+esString(cfg.From.UTC().Format(time.RFC3339Nano)))
	}
	if !cfg.To.IsZero() {
		conditions = append(conditions, timestamp+" <= "+esString(cfg.To.UTC().Format(time.RFC3339Nano)))
	}

	var builder strings.Builder
	builder.WriteString("FROM ")
	builder.WriteString(index)
	if len(conditions) > 0 {
		builder.WriteString(" | WHERE ")
		builder.WriteString(strings.Join(conditions, " AND "))
	}
	builder.WriteString(" | SORT ")
	builder.WriteString(timestamp)
	builder.WriteString(" DESC")
	if cfg.Limit > 0 {
		builder.WriteString(" | LIMIT ")
		builder.WriteString(strconv.Itoa(cfg.Limit))
	}
	return builder.String(), nil
}

func esField(schema map[string]string, name string) string {
	if mapped, ok := schema[strings.ToLower(name)]; ok && mapped != "" {
		return mapped
	}
	if name == "body" {
		return "body.text"
	}
	return "attributes." + name
}

// esString renders an ES|QL double-quoted string literal.
func esString(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\', '"':
			builder.WriteByte('\\')
			builder.WriteRune(r)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// likeContains renders a substring LIKE pattern, escaping ES wildcards.
func likeContains(value string) string {
	var builder strings.Builder
	builder.WriteByte('*')
	for _, r := range value {
		switch r {
		case '\\', '*', '?':
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	builder.WriteByte('*')
	return builder.String()
}

// anchorRegex makes a Prometheus/Loki-style regex match the whole field, since
// Loki matchers are fully anchored.
func anchorRegex(pattern string) string {
	pattern = strings.TrimPrefix(pattern, "^")
	pattern = strings.TrimSuffix(pattern, "$")
	return "^(?s:" + pattern + ")$"
}
