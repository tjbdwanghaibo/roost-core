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
// That is also why this implementation sweeps nothing by default and says so:
// a deployment supplies the owner set, and until it does, expired runs are
// resolved lazily by the next Enter from the same owner (see resolveClaim).
// Lazy resolution is correct but only fires when that owner comes back, so a
// deployment that wants resources released promptly wires the list here.
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
			owners := s.sweepOwners()
			if len(owners) == 0 {
				continue
			}
			resolved, err := service.Sweep(ctx, owners, SweepBatch)
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

// sweepOwners is the owner set this process sweeps.
//
// Empty by default, deliberately: there is no way to enumerate owners without
// an unbounded scan, and inventing one here would put that scan on a timer. A
// deployment that knows its live owners — from a presence service, from a
// shard's roster — supplies them.
func (s *Server) sweepOwners() []int64 { return nil }
