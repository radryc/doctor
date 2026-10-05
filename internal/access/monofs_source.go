package access

import (
	"context"
	"errors"
	"os"

	"github.com/radryc/monofs/pkg/authz"
)

// FileReader reads a file from MonoFS by logical path.
type FileReader interface {
	ReadFile(ctx context.Context, logicalPath string) ([]byte, error)
}

// MonoFSSource loads OWNERS files from MonoFS. The expected layout is
// "<dir>/.guardian/OWNERS" as produced by authz.OwnersPath.
type MonoFSSource struct {
	Reader FileReader
}

// LoadOwners implements OwnersSource.
func (s *MonoFSSource) LoadOwners(ctx context.Context, dir string) (*authz.OwnersFile, error) {
	if s.Reader == nil {
		return nil, nil
	}
	data, err := s.Reader.ReadFile(ctx, authz.OwnersPath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return authz.ParseOwners(data)
}

// Version implements OwnersSource. Version tokens are unavailable through the
// node read API, so cache entries expire by TTL.
func (s *MonoFSSource) Version(context.Context, string) (string, error) {
	return "", nil
}
