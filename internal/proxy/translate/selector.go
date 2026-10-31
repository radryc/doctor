// Package translate converts Doctor's reduced query forms into backend-native
// query languages.
package translate

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrUnsupported reports a construct the translator cannot express.
var ErrUnsupported = errors.New("translate: unsupported query construct")

// labelNamePattern rejects characters that would break out of an ES|QL field,
// a BigQuery column, or a CloudWatch Insights condition. Dots, @, /, and -
// are valid in the OTel/AWS label vocabulary.
var labelNamePattern = regexp.MustCompile(`^[A-Za-z_@][A-Za-z0-9_.@/-]*$`)

// Matcher is one parsed stream-selector label matcher.
type Matcher struct {
	Name  string
	Op    string // "=", "!=", "=~", "!~"
	Value string
}

// ParseSelector parses a LogQL stream selector like `{service="api", level!="debug"}`
// into label matchers.
func ParseSelector(selector string) ([]Matcher, error) {
	selector = strings.TrimSpace(selector)
	if !strings.HasPrefix(selector, "{") || !strings.HasSuffix(selector, "}") {
		return nil, fmt.Errorf("%w: stream selector must be brace-delimited: %q", ErrUnsupported, selector)
	}
	inner := strings.TrimSpace(selector[1 : len(selector)-1])
	if inner == "" {
		return nil, nil
	}
	matchers := make([]Matcher, 0)
	for _, part := range splitTopLevel(inner) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		matcher, err := parseMatcher(part)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, matcher)
	}
	return matchers, nil
}

func splitTopLevel(inner string) []string {
	parts := make([]string, 0)
	var (
		builder  strings.Builder
		inQuotes bool
		escaped  bool
	)
	for _, r := range inner {
		switch {
		case escaped:
			builder.WriteRune(r)
			escaped = false
		case r == '\\' && inQuotes:
			builder.WriteRune(r)
			escaped = true
		case r == '"':
			inQuotes = !inQuotes
			builder.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, builder.String())
			builder.Reset()
		default:
			builder.WriteRune(r)
		}
	}
	parts = append(parts, builder.String())
	return parts
}

func parseMatcher(part string) (Matcher, error) {
	for _, op := range []string{"=~", "!~", "!=", "="} {
		if idx := strings.Index(part, op); idx >= 0 {
			name := strings.TrimSpace(part[:idx])
			value := strings.TrimSpace(part[idx+len(op):])
			if name == "" {
				return Matcher{}, fmt.Errorf("%w: empty label name in %q", ErrUnsupported, part)
			}
			if !labelNamePattern.MatchString(name) {
				return Matcher{}, fmt.Errorf("%w: invalid label name %q", ErrUnsupported, name)
			}
			unquoted, err := unquote(value)
			if err != nil {
				return Matcher{}, err
			}
			return Matcher{Name: name, Op: op, Value: unquoted}, nil
		}
	}
	return Matcher{}, fmt.Errorf("%w: invalid matcher %q", ErrUnsupported, part)
}

func unquote(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("%w: matcher value must be quoted: %q", ErrUnsupported, value)
	}
	inner := value[1 : len(value)-1]
	var builder strings.Builder
	escaped := false
	for _, r := range inner {
		switch {
		case escaped:
			builder.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String(), nil
}
