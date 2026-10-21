// Package identity owns the canonical telemetry identity shared by Doctor's
// ingest, storage, and query projections.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// MaxServiceNameLength mirrors Guardian's canonical identity cap.
const MaxServiceNameLength = 200

// CanonicalServiceName builds <partition>.<intent>.<asset> and applies the
// same deterministic hash suffix as Guardian above MaxServiceNameLength.
func CanonicalServiceName(partition, intent, asset string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{partition, intent, asset} {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	full := strings.Join(parts, ".")
	if full == "" {
		return ""
	}
	if len(full) <= MaxServiceNameLength {
		return full
	}
	data, err := json.Marshal([]string{full})
	if err != nil {
		return full[:MaxServiceNameLength]
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])[:10]
	keep := MaxServiceNameLength - 1 - len(hash)
	if keep < 1 {
		keep = 1
	}
	prefix := strings.TrimRight(full[:keep], "._-")
	if prefix == "" {
		prefix = "service"
	}
	return prefix + "." + hash
}
