package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

var ErrConflict = errors.New("doctor/store: version conflict")

type Store interface {
	ReadFile(ctx context.Context, logicalPath string) ([]byte, error)
	ListDir(ctx context.Context, logicalDir string) ([]DirEntry, error)
	Stat(ctx context.Context, logicalPath string) (FileInfo, error)
	UpsertFiles(ctx context.Context, batch MutationBatch) (BatchRevision, error)
	DeletePaths(ctx context.Context, batch DeleteBatch) (BatchRevision, error)
	ListVersions(ctx context.Context, logicalPath string) ([]FileVersion, error)
	GetVersion(ctx context.Context, logicalPath, versionID string) (VersionedFile, error)
}

type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type FileInfo struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	VersionID string    `json:"version_id"`
	ModTime   time.Time `json:"mod_time"`
}

type MutationBatch struct {
	Writes  []PathWrite     `json:"writes"`
	Context MutationContext `json:"context"`
}

type PathWrite struct {
	LogicalPath       string `json:"logical_path"`
	Content           []byte `json:"content"`
	ExpectedVersionID string `json:"expected_version_id"`
}

type DeleteBatch struct {
	Deletes []PathDelete    `json:"deletes"`
	Context MutationContext `json:"context"`
}

type PathDelete struct {
	LogicalPath       string `json:"logical_path"`
	ExpectedVersionID string `json:"expected_version_id"`
}

type MutationContext struct {
	PrincipalID   string `json:"principal_id"`
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id"`
}

type BatchRevision struct {
	BatchRevisionID string        `json:"batch_revision_id"`
	Files           []FileVersion `json:"files"`
}

type FileVersion struct {
	LogicalPath     string    `json:"logical_path"`
	VersionID       string    `json:"version_id"`
	BatchRevisionID string    `json:"batch_revision_id"`
	ContentSHA256   string    `json:"content_sha256"`
	CommittedAt     time.Time `json:"committed_at"`
	Tombstone       bool      `json:"tombstone"`
	PrincipalID     string    `json:"principal_id"`
	Reason          string    `json:"reason"`
}

type VersionedFile struct {
	Version FileVersion `json:"version"`
	Content []byte      `json:"content"`
}

func NormalizeLogicalPath(logicalPath string) string {
	if logicalPath == "" {
		return "/"
	}
	clean := path.Clean(logicalPath)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	return clean
}

func ContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func NewVersionID() string {
	return "ver_" + time.Now().UTC().Format("20060102T150405.000000000") + "_" + randomHex(6)
}

func NewBatchRevisionID() string {
	return "batch_" + time.Now().UTC().Format("20060102T150405.000000000") + "_" + randomHex(6)
}

func randomHex(bytesN int) string {
	if bytesN <= 0 {
		bytesN = 6
	}
	buf := make([]byte, bytesN)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
