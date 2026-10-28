package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"path"
	"sort"
	"time"

	"github.com/radryc/doctor/internal/store"
)

// TokenBundleWriter publishes the sharded MonoFS token fallback used when the
// Guardian lazy resolver is unavailable. Only changed shards are rewritten.
type TokenBundleWriter struct {
	Files      CatalogFileStore
	Token      string
	Root       string
	Tenant     string
	ShardCount int
	Principal  string
}

// NewTokenBundleWriter builds a token bundle writer.
func NewTokenBundleWriter(files CatalogFileStore, token, root, tenant string, shardCount int) *TokenBundleWriter {
	if root == "" {
		root = "/doctor/v1/federation/tokens"
	}
	if tenant == "" {
		tenant = "default"
	}
	if shardCount <= 0 {
		shardCount = 256
	}
	return &TokenBundleWriter{
		Files:      files,
		Token:      token,
		Root:       "/" + trimSlashes(root),
		Tenant:     tenant,
		ShardCount: shardCount,
		Principal:  "doctor-federation-controller",
	}
}

// Write publishes tokens grouped by target ID. It returns the number of shards
// changed. Tokens are never written to the catalog snapshot.
func (w *TokenBundleWriter) Write(ctx context.Context, tokens map[string]string) (int, error) {
	if w == nil || w.Files == nil {
		return 0, fmt.Errorf("token bundle file store is required")
	}
	shards := make(map[int]map[string]string)
	for targetID, token := range tokens {
		if targetID == "" || token == "" {
			continue
		}
		shard := w.shardFor(targetID)
		if shards[shard] == nil {
			shards[shard] = make(map[string]string)
		}
		shards[shard][targetID] = token
	}
	changed := 0
	for shard, values := range shards {
		changedShard, err := w.writeShard(ctx, shard, values)
		if err != nil {
			return changed, err
		}
		if changedShard {
			changed++
		}
	}
	return changed, nil
}

func (w *TokenBundleWriter) shardFor(targetID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(targetID))
	return int(h.Sum32() % uint32(w.ShardCount))
}

func (w *TokenBundleWriter) shardPath(shard int) string {
	return path.Join(w.Root, w.Tenant, fmt.Sprintf("%04d.json", shard))
}

func (w *TokenBundleWriter) writeShard(ctx context.Context, shard int, tokens map[string]string) (bool, error) {
	logicalPath := w.shardPath(shard)
	if existing, err := w.Files.ReadFile(ctx, logicalPath); err == nil {
		var current struct {
			Tokens map[string]string `json:"tokens"`
		}
		if json.Unmarshal(existing, &current) == nil && equalStringMaps(current.Tokens, tokens) {
			return false, nil
		}
	}
	body, err := json.Marshal(struct {
		Version string            `json:"version"`
		Tokens  map[string]string `json:"tokens"`
	}{
		Version: time.Now().UTC().Format(time.RFC3339Nano),
		Tokens:  tokens,
	})
	if err != nil {
		return false, err
	}
	expected := "absent"
	if versions, err := w.Files.ListVersions(ctx, w.Token, logicalPath); err == nil && len(versions) > 0 {
		expected = versions[0].VersionID
	}
	_, err = w.Files.UpsertPaths(ctx, w.Token, []store.PathWrite{{
		LogicalPath:       logicalPath,
		Content:           body,
		ExpectedVersionID: expected,
	}}, store.MutationContext{PrincipalID: w.Principal, Reason: "federation token bundle publish"})
	if err != nil {
		return false, fmt.Errorf("write token shard %s: %w", logicalPath, err)
	}
	return true, nil
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	keys := make([]string, 0, len(left))
	for key := range left {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var leftBuf, rightBuf bytes.Buffer
	for _, key := range keys {
		leftBuf.WriteString(key)
		leftBuf.WriteByte(0)
		leftBuf.WriteString(left[key])
		leftBuf.WriteByte(0)
		rightBuf.WriteString(key)
		rightBuf.WriteByte(0)
		rightBuf.WriteString(right[key])
		rightBuf.WriteByte(0)
	}
	return bytes.Equal(leftBuf.Bytes(), rightBuf.Bytes())
}

func trimSlashes(value string) string {
	for len(value) > 0 && value[0] == '/' {
		value = value[1:]
	}
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}
