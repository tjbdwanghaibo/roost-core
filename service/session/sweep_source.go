package session

import (
	"context"
	"fmt"
)

func (s *Service) BackgroundSweepEnabled() bool { return s != nil && s.cfg.Owners != nil }

// SweepPending is the owner-only hook used by the kit server. A source error
// leaves progress to the next tick, and oversized pages are refused.
func (s *Service) SweepPending(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > MaxPageSize {
		return nil, fmt.Errorf("%w: sweep limit", ErrRangeInvalid)
	}
	if !s.BackgroundSweepEnabled() {
		return nil, nil
	}
	owners, err := s.cfg.Owners.SweepOwners(ctx, limit)
	if err != nil {
		return nil, err
	}
	if len(owners) > limit {
		return nil, fmt.Errorf("%w: owner source exceeds requested limit", ErrRangeInvalid)
	}
	return s.Sweep(ctx, owners, limit)
}
