package quota

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

var partitionMatcher = regexp.MustCompile(`guardian[._]partition\s*=\s*"([^"]+)"`)

// Middleware applies query admission after authentication and before the
// handler. It never limits non-API assets.
func (e *Engine) Middleware(next http.Handler) http.Handler {
	if e == nil || e.provider == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ShouldLimit(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		id, _ := authz.IdentityFromContext(r.Context())
		partitions := PartitionsFromRequest(r)
		limits := e.provider.QueryLimits(id, partitions)
		enforce := e.enforceRequest(id, partitions)
		if maxRange, err := policy.ParseQuotaDuration(limits.MaxRange); err == nil && maxRange > 0 && enforce {
			if window, _, ok := QueryWindow(r); ok && window > maxRange {
				writeQuotaRejection(w, &LimitError{
					Key:      "query:range",
					Resource: "query.range",
					Limit:    maxRange.Seconds(),
				}, true)
				return
			}
		}
		if limits.MaxSamples > 0 && enforce {
			if _, samples, ok := QueryWindow(r); ok && samples > float64(limits.MaxSamples) {
				writeQuotaRejection(w, &LimitError{
					Key:      "query:samples",
					Resource: "query.samples",
					Limit:    float64(limits.MaxSamples),
				}, true)
				return
			}
		}
		if max := limits.MaxPartitions; max > 0 && len(partitions) > max && enforce {
			writeQuotaRejection(w, &LimitError{
				Key:      "query:partitions",
				Resource: "query.partitions",
				Limit:    float64(max),
			}, true)
			return
		}
		release, err := e.AcquireQuery(r.Context(), QueryRequest{
			Identity:   id,
			Partitions: partitions,
			Cost:       QueryCost(r),
		})
		if err != nil {
			var limitErr *LimitError
			if errors.As(err, &limitErr) {
				writeQuotaRejection(w, limitErr, e.provider.Enforce(firstPartition(partitions)))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

func (e *Engine) enforceRequest(id authz.Identity, partitions []string) bool {
	if e == nil || e.provider == nil {
		return false
	}
	enforce := e.provider.Enforce("")
	if len(partitions) > 0 {
		enforce = false
		for _, partition := range partitions {
			if e.provider.Enforce(partition) {
				enforce = true
				break
			}
		}
	}
	for _, scoped := range e.provider.SubjectQuotas(id) {
		if strings.EqualFold(scoped.Quota.Enforcement, "enforce") {
			enforce = true
			break
		}
	}
	return enforce
}

// ShouldLimit reports whether a path is a read API that should consume quota.
func ShouldLimit(path string) bool {
	switch {
	case path == "/healthz", path == "/ready", path == "/metrics",
		strings.HasPrefix(path, "/static/"), strings.HasPrefix(path, "/auth/"),
		strings.HasPrefix(path, "/v1/ui/"), path == "/v1/quota":
		return false
	case strings.HasPrefix(path, "/api/"), strings.HasPrefix(path, "/v1/"),
		strings.HasPrefix(path, "/loki/"), strings.HasPrefix(path, "/tempo/"):
		return true
	default:
		return false
	}
}

// PartitionsFromRequest extracts the partitions a query touches. It looks at
// explicit query parameters, PromQL/Loki label matchers, and canonical service
// names.
func PartitionsFromRequest(r *http.Request) []string {
	if r == nil {
		return nil
	}
	values := r.URL.Query()
	seen := map[string]struct{}{}
	out := make([]string, 0, 2)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	add(values.Get("partition"))
	add(values.Get("guardian_partition"))
	add(values.Get("guardian.partition"))
	for _, key := range []string{"query", "q"} {
		raw := values.Get(key)
		if raw == "" {
			continue
		}
		for _, match := range partitionMatcher.FindAllStringSubmatch(raw, -1) {
			if len(match) > 1 {
				add(match[1])
			}
		}
	}
	for _, key := range []string{"service", "serviceName", "service_name"} {
		if partition, _, _, ok := splitCanonical(values.Get(key)); ok {
			add(partition)
		}
	}
	return out
}

func splitCanonical(value string) (partition, intent, asset string, ok bool) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return "", "", "", false
		}
	}
	return parts[0], parts[1], parts[2], true
}

// QueryCost estimates work in 1k-sample units. Fixed requests cost one unit.
func QueryCost(r *http.Request) float64 {
	if r == nil {
		return 1
	}
	if _, samples, ok := QueryWindow(r); ok {
		return clampCost(samples / 1000)
	}
	values := r.URL.Query()
	if r.URL.Path == "/api/search" || strings.HasPrefix(r.URL.Path, "/loki/api/v1") {
		if window, ok := parseLookback(values.Get("since")); ok {
			return clampCost(window.Hours())
		}
	}
	return 1
}

// QueryWindow returns the requested time range and estimated samples for range
// queries. It returns ok=false for instant or non-range requests.
func QueryWindow(r *http.Request) (time.Duration, float64, bool) {
	if r == nil {
		return 0, 0, false
	}
	values := r.URL.Query()
	from, okFrom := parseTimeParam(values.Get("start"))
	if !okFrom {
		from, okFrom = parseTimeParam(values.Get("from"))
	}
	to, okTo := parseTimeParam(values.Get("end"))
	if !okTo {
		to, okTo = parseTimeParam(values.Get("to"))
	}
	if !okFrom || !okTo || !to.After(from) {
		if window, ok := parseLookback(values.Get("since")); ok {
			return window, window.Seconds() / 15, true
		}
		return 0, 0, false
	}
	step, ok := parseStepParam(values.Get("step"))
	if !ok {
		step, ok = parseStepParam(values.Get("interval"))
	}
	if !ok || step <= 0 {
		step = 15 * time.Second
	}
	window := to.Sub(from)
	samples := window.Seconds() / step.Seconds()
	return window, samples, true
}

func parseTimeParam(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if integer, err := strconv.ParseInt(raw, 10, 64); err == nil {
		switch {
		case len(raw) >= 19: // Unix nanoseconds
			return time.Unix(0, integer), true
		case len(raw) >= 16: // Unix microseconds
			return time.Unix(0, integer*int64(time.Microsecond)), true
		case len(raw) >= 13: // Unix milliseconds
			return time.Unix(0, integer*int64(time.Millisecond)), true
		default: // Unix seconds
			return time.Unix(integer, 0), true
		}
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Unix(0, int64(seconds*float64(time.Second))), true
	}
	return time.Time{}, false
}

func parseStepParam(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d, true
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(seconds * float64(time.Second)), true
	}
	return 0, false
}

func parseLookback(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d, true
	}
	return 0, false
}

func clampCost(value float64) float64 {
	if value < 1 {
		return 1
	}
	if value > 1000 {
		return 1000
	}
	return value
}

func firstPartition(partitions []string) string {
	if len(partitions) == 0 {
		return ""
	}
	return partitions[0]
}

func writeQuotaRejection(w http.ResponseWriter, err *LimitError, enforce bool) {
	status := http.StatusTooManyRequests
	if !enforce {
		// Observe mode never reaches here, but keep the helper safe.
		status = http.StatusOK
	}
	retry := int(err.RetryAfter.Seconds())
	if retry < 1 {
		retry = 1
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("X-RateLimit-Limit", formatFloat(err.Limit))
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(err.RetryAfter).Unix(), 10))
	w.Header().Set("X-Doctor-Quota-Bucket", err.Key)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"status":"error","errorType":"rate_limited","error":"` + escapeJSON(err.Error()) + `"}`))
}

// WriteLimitError writes a quota rejection for callers outside this package,
// such as a response-series guard in the query handlers.
func WriteLimitError(w http.ResponseWriter, err *LimitError) {
	writeQuotaRejection(w, err, true)
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func escapeJSON(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
}

// QueryPartitionsFromValues is exported for tests and non-HTTP callers.
func QueryPartitionsFromValues(values url.Values) []string {
	r := &http.Request{URL: &url.URL{RawQuery: values.Encode()}}
	return PartitionsFromRequest(r)
}
