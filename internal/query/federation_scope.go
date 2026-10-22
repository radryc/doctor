package query

import (
	"context"

	"github.com/rydzu/ainfra/doctor/internal/federation"
)

// checkAWSLogScope rejects a live AWS log query before the router fans it out
// when the federation manager cannot resolve a bounded target set.
func (s *Service) checkAWSLogScope(ctx context.Context, query string, logGroups []string) error {
	if s == nil || s.aws == nil {
		return nil
	}
	checker, ok := s.aws.(federation.ScopeChecker)
	if !ok {
		return nil
	}
	return checker.CheckScope(ctx, federation.Query{
		Signal:    federation.SignalLogs,
		Expr:      query,
		LogGroups: logGroups,
	})
}
