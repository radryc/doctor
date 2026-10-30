package alerting

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// Fingerprint returns a stable identity for an alert. Volatile fields such as
// timestamps, values, and annotations are deliberately excluded.
func Fingerprint(source, tenant, partition string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		if key == "__name__" || key == "value" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(source)
	b.WriteByte(0)
	b.WriteString(tenant)
	b.WriteByte(0)
	b.WriteString(partition)
	b.WriteByte(0)
	for _, key := range keys {
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(labels[key])
		b.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func regexMatch(pattern, value string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}
