package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// AdmissionSource 由 run 存储实现，索引与 AdmissionPending 同一原子写维护。
// 候选只用于定位；删除还要检查当前值和版本，不能因陈旧索引删除已准入 run。
type AdmissionSource interface {
	PendingAdmissions(ctx context.Context, nowUnix int64, limit int) ([]string, error)
	versionstore.ConditionalDeleter[string, Run]
}

func (s *Service) BackgroundSweepEnabled() bool {
	if s == nil {
		return false
	}
	_, indexed := s.cfg.Runs.(AdmissionSource)
	return indexed || s.cfg.Owners != nil
}

// SweepPending is the owner-only hook used by the kit server. A source error
// leaves progress to the next tick, and oversized pages are refused.
func (s *Service) SweepPending(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > MaxPageSize {
		return nil, fmt.Errorf("%w: sweep limit", ErrRangeInvalid)
	}
	var admissionErr error
	if source, ok := s.cfg.Runs.(AdmissionSource); ok {
		admissionErr = s.sweepAdmissions(ctx, source, limit)
	}
	if s.cfg.Owners == nil {
		return nil, admissionErr
	}
	owners, err := s.cfg.Owners.SweepOwners(ctx, limit)
	if err != nil {
		return nil, errors.Join(admissionErr, err)
	}
	if len(owners) > limit {
		return nil, fmt.Errorf("%w: owner source exceeds requested limit", ErrRangeInvalid)
	}
	runs, err := s.Sweep(ctx, owners, limit)
	return runs, errors.Join(admissionErr, err)
}

func (s *Service) sweepAdmissions(ctx context.Context, source AdmissionSource, limit int) error {
	nowUnix := s.cfg.Now().Unix()
	ids, err := source.PendingAdmissions(ctx, nowUnix, limit)
	if err != nil {
		return err
	}
	if len(ids) > limit {
		return fmt.Errorf("%w: admission source exceeds requested limit", ErrRangeInvalid)
	}
	var failures error
	for _, id := range ids {
		if err := s.reclaimAdmission(ctx, source, id, nowUnix); err != nil {
			failures = errors.Join(failures, fmt.Errorf("session admission %s: %w", id, err))
			// 坏记录或暂时不可用的外部存储不能永久占满索引首页。
			if store, ok := s.cfg.Runs.(*redisRuns); ok {
				failures = errors.Join(failures, store.deferAdmission(ctx, id, nowUnix+60))
			}
		}
	}
	return failures
}

func (s *Service) reclaimAdmission(ctx context.Context, source AdmissionSource, id string, nowUnix int64) error {
	stored, found, err := s.cfg.Runs.Get(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		if store, ok := s.cfg.Runs.(*redisRuns); ok {
			return store.removeAbsentAdmission(ctx, id)
		}
		return nil
	}
	eligible := func(run Run) bool {
		return run.ID == id && run.AdmissionPending && run.Expired(nowUnix) && len(run.Resources) == 0
	}
	if !eligible(stored.Value) {
		return nil
	}
	if err := source.DeleteIf(ctx, id, stored, eligible); err != nil {
		if errors.Is(err, versionstore.ErrVersionMismatch) {
			return nil
		}
		return err
	}
	s.report.Dropped("admission.reclaimed", 1)
	return s.releaseClaim(ctx, stored.Value.OwnerID, id)
}
