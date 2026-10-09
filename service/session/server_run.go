package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// SweepInterval is how often the session process resolves expired runs.
//
// A run that lapsed holds external resources — a scene, a replica — and
// nothing else releases them, so this cadence is the difference between a
// deadline that means something and a deadline that is a decoration. The
// implementation this replaces computed a deadline, stored it, sent it to
// clients, and never compared it to a clock anywhere.
const SweepInterval = 30 * time.Second

// SweepBatch bounds one sweep, so a backlog is worked through in bounded
// steps rather than in one unbounded pass that holds a connection for however
// long it takes.
const SweepBatch = 100

// run resolves expired runs until the process is shutting down.
//
// It is hand-written because the work is this service's own: which owners to
// sweep is a question only the caller can answer — the sweep takes an owner
// list, because scanning every owner in the store is the unbounded read this
// repository exists to remove.
//
// Redis 默认按有界 admission 索引回收尚未完成准入、也未挂载资源的孤儿 Run。
// 已准入 Run 的资源回收仍由 owner 集合驱动；部署方通过 WithSweepOwners 提供集合，
// 未提供时由同 owner 的下一次 Enter 惰性推进。二者不能混为“默认全量扫描”。
func (s *Server) run(ctx context.Context) error {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	// Resolved ONCE, before the loop, and that placement is the lesson rather
	// than a tidy-up. This used to be a bare `s.Service().(*Service)` inside
	// the ticker body — so when the owning Mod briefly published a capability
	// WRAPPER under the owner-only name, this did not fail at startup: it
	// panicked thirty seconds in, and only in a deployment that had actually
	// supplied owners to sweep. A lazy assertion moves a wiring error out of
	// startup and into production traffic.
	service, ok := s.Service().(*Service)
	if !ok {
		// The Server only starts on the local implementation, so this cannot
		// happen — and if it ever does, sweeping nothing silently is how a run
		// deadline stops being enforced with nothing failing.
		return fmt.Errorf("session server: the local capability is not a *Service, so no run deadline is being enforced")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			resolved, err := service.SweepPending(ctx, SweepBatch)
			if err != nil {
				// Reported, not returned: a sweep that failed is retried on
				// the next tick, and taking the process down for it would
				// turn a recoverable backlog into an outage.
				slog.Error("session server: sweep failed", "err", err, "resolved", len(resolved))
				continue
			}
			if len(resolved) > 0 {
				slog.Info("session server: resolved expired runs", "count", len(resolved))
			}
		}
	}
}
