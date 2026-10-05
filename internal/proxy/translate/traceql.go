package translate

import (
	"fmt"
	"strings"
)

// TraceTag is one exact-match trace tag filter.
type TraceTag struct {
	Key   string
	Value string
}

// TraceQuery is a translated TraceQL subset request.
type TraceQuery struct {
	Service string
	Tags    []TraceTag
}

// ParseTraceQL parses the supported TraceQL subset: a single brace expression
// with `key = "value"` matchers joined by `&&`. Supported keys include
// `.name`, `.service.name`, `resource.service.name`, `.status`, `.kind`, and
// bare attribute names.
func ParseTraceQL(query string) (TraceQuery, error) {
	query = strings.TrimSpace(query)
	if !strings.HasPrefix(query, "{") || !strings.HasSuffix(query, "}") {
		return TraceQuery{}, fmt.Errorf("%w: traceql must be a brace expression: %q", ErrUnsupported, query)
	}
	inner := strings.TrimSpace(query[1 : len(query)-1])
	if inner == "" {
		return TraceQuery{}, fmt.Errorf("%w: empty traceql expression", ErrUnsupported)
	}

	result := TraceQuery{}
	for _, clause := range strings.Split(inner, "&&") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		key, value, err := parseTraceQLClause(clause)
		if err != nil {
			return TraceQuery{}, err
		}
		key = strings.TrimPrefix(key, ".")
		switch key {
		case "name", "span.name":
			result.Tags = append(result.Tags, TraceTag{Key: "name", Value: value})
		case "service.name", "resource.service.name", "service":
			if result.Service != "" {
				return TraceQuery{}, fmt.Errorf("%w: multiple service filters", ErrUnsupported)
			}
			result.Service = value
		case "status", "span.status":
			result.Tags = append(result.Tags, TraceTag{Key: "status", Value: value})
		case "kind", "span.kind":
			result.Tags = append(result.Tags, TraceTag{Key: "kind", Value: value})
		default:
			result.Tags = append(result.Tags, TraceTag{Key: key, Value: value})
		}
	}
	if result.Service == "" && len(result.Tags) == 0 {
		return TraceQuery{}, fmt.Errorf("%w: no supported matchers in %q", ErrUnsupported, query)
	}
	return result, nil
}

func parseTraceQLClause(clause string) (string, string, error) {
	idx := strings.Index(clause, "=")
	if idx <= 0 {
		return "", "", fmt.Errorf("%w: unsupported traceql matcher %q", ErrUnsupported, clause)
	}
	key := strings.TrimSpace(clause[:idx])
	raw := strings.TrimSpace(clause[idx+1:])
	if strings.HasPrefix(raw, "~") {
		return "", "", fmt.Errorf("%w: regex traceql matchers are not supported: %q", ErrUnsupported, clause)
	}
	if key == "" || !strings.HasPrefix(raw, `"`) || !strings.HasSuffix(raw, `"`) || len(raw) < 2 {
		return "", "", fmt.Errorf("%w: invalid traceql matcher %q", ErrUnsupported, clause)
	}
	value := strings.ReplaceAll(raw[1:len(raw)-1], `\"`, `"`)
	return key, value, nil
}
