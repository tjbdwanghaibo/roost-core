package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var ErrLoadWaitersExceeded = errors.New("cache: load waiter limit exceeded")

type Loader[K comparable, V any] func(context.Context, K) (V, bool, error)

type ReadThroughOptions struct {
	LocalTTL time.Duration
	// LoadTimeout bounds one authoritative load, including the L2 read and
	// the L2 back-fill performed on that path.
	LoadTimeout      time.Duration
	MaxWaitersPerKey int
	// RemoteTimeout bounds the L2 call made by Set and Delete. It exists
	// because those are the calls a caller cannot bound for itself: a
	// write-through happens while the caller holds whatever lock serialises
	// its publish, so an unresponsive L2 does not fail the write — it pins
	// that lock for as long as the L2 stays unresponsive. Zero falls back to
	// LoadTimeout; if both are zero the call is unbounded, which is only safe
	// when the caller's own context carries a deadline.
	RemoteTimeout     time.Duration
	IgnoreRemoteError bool
	// FatalRemoteError classifies L2 errors that IgnoreRemoteError must NOT
	// degrade around. IgnoreRemoteError exists for outages — L2 unreachable,
	// keep L1 usable. A remote store can also answer with a consistency
	// verdict (this version already holds a different value); treating that
	// as an outage writes the rejected value into L1 and reports success
	// (RR-20260913-05 复核). Optional; nil means every error is an outage.
	FatalRemoteError func(error) bool
}

type ReadThroughStats struct {
	Loads       uint64
	Coalesced   uint64
	LoadErrors  uint64
	RemoteError uint64
}

type readThroughCall[V any] struct {
	done    chan struct{}
	value   V
	ok      bool
	err     error
	waiters int
}

// ReadThroughStore composes L1, optional L2, and an authoritative loader. It
// coalesces concurrent misses per key without spawning a goroutine per load.
type ReadThroughStore[K comparable, V any] struct {
	cfg    StoreConfig[K, V]
	local  Store[K, V]
	remote Store[K, V]
	loader Loader[K, V]
	opts   ReadThroughOptions

	mu    sync.Mutex
	calls map[K]*readThroughCall[V]

	loads       atomic.Uint64
	coalesced   atomic.Uint64
	loadErrors  atomic.Uint64
	remoteError atomic.Uint64
}

func NewReadThroughStore[K comparable, V any](local Store[K, V], remote Store[K, V], loader Loader[K, V], cfg StoreConfig[K, V], opts ReadThroughOptions) *ReadThroughStore[K, V] {
	if opts.MaxWaitersPerKey <= 0 {
		opts.MaxWaitersPerKey = 256
	}
	return &ReadThroughStore[K, V]{cfg: cfg, local: local, remote: remote, loader: loader, opts: opts, calls: make(map[K]*readThroughCall[V])}
}

func (s *ReadThroughStore[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	var zero V
	if s == nil || !s.cfg.validKey(key) {
		return zero, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.local != nil {
		value, ok, err := s.local.Get(ctx, key)
		if err != nil || ok {
			return value, ok, err
		}
	}
	return s.load(ctx, key)
}

func (s *ReadThroughStore[K, V]) load(ctx context.Context, key K) (V, bool, error) {
	s.mu.Lock()
	if call := s.calls[key]; call != nil {
		if call.waiters >= s.opts.MaxWaitersPerKey {
			s.mu.Unlock()
			var zero V
			return zero, false, ErrLoadWaitersExceeded
		}
		call.waiters++
		s.coalesced.Add(1)
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.value, call.ok, call.err
		case <-ctx.Done():
			// Give the slot back: the limit counts live followers, not entries.
			// Only if this is still the same load — a finished load is gone from
			// the map, and a newer one for the key must not be debited.
			s.mu.Lock()
			if s.calls[key] == call {
				call.waiters--
			}
			s.mu.Unlock()
			var zero V
			return zero, false, ctx.Err()
		}
	}
	call := &readThroughCall[V]{done: make(chan struct{})}
	s.calls[key] = call
	s.mu.Unlock()

	loadCtx := ctx
	var cancel context.CancelFunc
	if s.opts.LoadTimeout > 0 {
		loadCtx, cancel = context.WithTimeout(ctx, s.opts.LoadTimeout)
		defer cancel()
	}
	call.value, call.ok, call.err = s.loadOne(loadCtx, key)
	if call.err != nil {
		s.loadErrors.Add(1)
	}
	s.mu.Lock()
	delete(s.calls, key)
	close(call.done)
	s.mu.Unlock()
	return call.value, call.ok, call.err
}

func (s *ReadThroughStore[K, V]) loadOne(ctx context.Context, key K) (V, bool, error) {
	var zero V
	if s.remote != nil {
		value, ok, err := s.remote.Get(ctx, key)
		if err != nil {
			s.remoteError.Add(1)
			if !s.opts.IgnoreRemoteError {
				return zero, false, err
			}
		} else if ok {
			// Backfilling is a write like any other and goes through the same
			// Stale / Conflict rule as a publish. If L1 meanwhile took a
			// DIFFERENT value for this same version, L1 is what this process
			// has already handed out; the L2 copy does not get to silently
			// replace it, and the caller sees what L1 holds (RR-20260913-06).
			switch err := s.setLocal(ctx, value); {
			case errors.Is(err, ErrConflictingWrite):
				if current, held, getErr := s.local.Get(ctx, key); getErr == nil && held {
					return current, true, nil
				}
			case errors.Is(err, ErrStaleWrite):
				// L1 refused the backfill as superseded (a delete recorded at a
				// newer version while this read was in flight, RR-20260913-01
				// 复核). The captured L2 value is the past: hand out what L1
				// holds, or a miss — never the value L1 just refused.
				if current, held, getErr := s.local.Get(ctx, key); getErr == nil && held {
					return current, true, nil
				}
				return zero, false, nil
			}
			return value, true, nil
		}
	}
	if s.loader == nil {
		return zero, false, nil
	}
	s.loads.Add(1)
	value, ok, err := s.loader(ctx, key)
	if err != nil || !ok {
		return value, ok, err
	}
	if s.remote != nil {
		if err := s.remote.Set(ctx, value); err != nil && !s.degradable(err) {
			return zero, false, err
		}
	}
	if err := s.setLocal(ctx, value); err != nil {
		return zero, false, err
	}
	return value, true, nil
}

// degradable reports whether an L2 error may be absorbed under
// IgnoreRemoteError: outages yes, consistency verdicts never.
func (s *ReadThroughStore[K, V]) degradable(err error) bool {
	if !s.opts.IgnoreRemoteError {
		return false
	}
	return s.opts.FatalRemoteError == nil || !s.opts.FatalRemoteError(err)
}

func (s *ReadThroughStore[K, V]) Set(ctx context.Context, value V) error {
	if s == nil {
		return nil
	}
	if s.cfg.ValidateValue != nil {
		if err := s.cfg.ValidateValue(value); err != nil {
			return err
		}
	}
	if _, err := s.cfg.keyOf(value); err != nil {
		return err
	}
	if s.remote != nil {
		remoteCtx, cancel := s.remoteContext(ctx)
		defer cancel()
		err := s.remote.Set(remoteCtx, value)
		if err != nil {
			s.remoteError.Add(1)
			if !s.degradable(err) {
				return err
			}
		}
	}
	return s.setLocal(ctx, value)
}

// remoteContext bounds one L2 call. The returned cancel is always safe to
// call and should be deferred: the bounded context is dead once the remote
// call returns, so deferring costs nothing and keeps the context's timer from
// outliving a panic in the store.
func (s *ReadThroughStore[K, V]) remoteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.opts.RemoteTimeout
	if timeout <= 0 {
		timeout = s.opts.LoadTimeout
	}
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *ReadThroughStore[K, V]) Delete(ctx context.Context, key K) error {
	if s == nil || !s.cfg.validKey(key) {
		return nil
	}
	var joined error
	if s.remote != nil {
		remoteCtx, cancel := s.remoteContext(ctx)
		defer cancel()
		if err := s.remote.Delete(remoteCtx, key); err != nil {
			s.remoteError.Add(1)
			if !s.opts.IgnoreRemoteError {
				joined = errors.Join(joined, err)
			}
		}
	}
	if s.local != nil {
		joined = errors.Join(joined, s.local.Delete(ctx, key))
	}
	return joined
}

func (s *ReadThroughStore[K, V]) Stats() ReadThroughStats {
	if s == nil {
		return ReadThroughStats{}
	}
	return ReadThroughStats{Loads: s.loads.Load(), Coalesced: s.coalesced.Load(), LoadErrors: s.loadErrors.Load(), RemoteError: s.remoteError.Load()}
}

func (s *ReadThroughStore[K, V]) setLocal(ctx context.Context, value V) error {
	if s.local == nil {
		return nil
	}
	if expiring, ok := s.local.(ExpiringStore[K, V]); ok {
		return expiring.SetWithTTL(ctx, value, s.opts.LocalTTL)
	}
	return s.local.Set(ctx, value)
}

var _ Store[int, int] = (*ReadThroughStore[int, int])(nil)
