package federation

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path"
	"sort"
	"time"

	"github.com/radryc/doctor/internal/store"
)

// tokenShardDocument is the on-disk token bundle shard format. New writers
// emit Ciphertext when a key is configured; Tokens remains supported so older
// plaintext shards can be read and migrated.
type tokenShardDocument struct {
	Version    string            `json:"version"`
	Tokens     map[string]string `json:"tokens,omitempty"`
	Ciphertext string            `json:"ciphertext,omitempty"`
}

// TokenBundleWriter publishes the sharded MonoFS token fallback used when the
// Guardian lazy resolver is unavailable. Only changed shards are rewritten.
type TokenBundleWriter struct {
	Files      CatalogFileStore
	Token      string
	Root       string
	Tenant     string
	ShardCount int
	Principal  string
	// Key encrypts token maps with AES-256-GCM when non-empty. It is the raw
	// operator secret; the actual key is derived with SHA-256.
	Key []byte
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
	// Iterate every configured shard, not just the shards present in the new
	// map. Otherwise a removed target leaves its old plaintext token in an
	// otherwise-untouched shard and the resolver keeps serving a revoked
	// credential until the shard is rewritten for another target.
	for shard := 0; shard < w.ShardCount; shard++ {
		changedShard, err := w.writeShard(ctx, shard, shards[shard])
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
	if tokens == nil {
		tokens = map[string]string{}
	}
	existing, readErr := w.Files.ReadFile(ctx, logicalPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, fmt.Errorf("read token shard %s: %w", logicalPath, readErr)
	}
	if readErr == nil {
		var current tokenShardDocument
		if err := json.Unmarshal(existing, &current); err == nil {
			currentTokens, decodeErr := decodeTokenShard(current, w.Key)
			if decodeErr == nil && equalStringMaps(currentTokens, tokens) {
				// Migrate legacy plaintext shards to the configured
				// encryption instead of treating them as unchanged.
				if len(w.Key) == 0 || current.Ciphertext != "" {
					return false, nil
				}
			}
			if decodeErr != nil && current.Ciphertext != "" && len(w.Key) == 0 {
				return false, fmt.Errorf("token shard %s is encrypted but DOCTOR_FEDERATION_TOKEN_KEY is unset", logicalPath)
			}
			if decodeErr == nil && len(tokens) == 0 && len(currentTokens) == 0 {
				return false, nil
			}
		}
	} else if len(tokens) == 0 {
		// Missing shard with no new tokens: nothing to publish.
		return false, nil
	}
	doc := tokenShardDocument{Version: time.Now().UTC().Format(time.RFC3339Nano)}
	if len(w.Key) > 0 {
		ciphertext, encryptErr := encryptTokenMap(tokens, w.Key)
		if encryptErr != nil {
			return false, encryptErr
		}
		doc.Ciphertext = ciphertext
	} else {
		doc.Tokens = tokens
	}
	body, err := json.Marshal(doc)
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

func deriveTokenKey(key []byte) []byte {
	sum := sha256.Sum256(key)
	return sum[:]
}

func encryptTokenMap(tokens map[string]string, key []byte) (string, error) {
	plain, err := json.Marshal(tokens)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(deriveTokenKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptTokenMap(ciphertext string, key []byte) (map[string]string, error) {
	if len(key) == 0 {
		return nil, errors.New("token bundle is encrypted but no decryption key is configured")
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode token ciphertext: %w", err)
	}
	block, err := aes.NewCipher(deriveTokenKey(key))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("token ciphertext is too short")
	}
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt token bundle: %w", err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(plain, &tokens); err != nil {
		return nil, fmt.Errorf("decode token bundle: %w", err)
	}
	if tokens == nil {
		tokens = map[string]string{}
	}
	return tokens, nil
}

// decodeTokenShard returns the plaintext token map for a shard document.
// Plaintext documents are accepted for backwards compatibility; encrypted
// documents require the configured key.
func decodeTokenShard(doc tokenShardDocument, key []byte) (map[string]string, error) {
	if doc.Ciphertext != "" {
		return decryptTokenMap(doc.Ciphertext, key)
	}
	if doc.Tokens == nil {
		return map[string]string{}, nil
	}
	return doc.Tokens, nil
}
