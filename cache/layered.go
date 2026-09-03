package cache

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"sync"
	"time"
)

type LayeredStore[K comparable, V any] struct {
	cfg    StoreConfig[K, V]
	local  Store[K, V]
	remote Store[K, V]
	ttl    time.Duration

	mu     sync.Mutex
	expiry map[K]time.Time
}

func NewLayeredStore[K comparable, V any](local Store[K, V], remote Store[K, V], ttl time.Duration, cfg StoreConfig[K, V]) *LayeredStore[K, V] {
	return &LayeredStore[K, V]{
		cfg:    cfg,
		local:  local,
		remote: remote,
		ttl:    ttl,
		expiry: make(map[K]time.Time),
	}
}

func (s *LayeredStore[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	var zero V
	if s == nil || !s.cfg.validKey(key) {
		return zero, false, nil
	}
	if s.local != nil {
		if s.remote == nil || s.localValid(key, time.Now()) {
			value, ok, err := s.local.Get(ctx, key)
			if err != nil {
				return zero, false, err
			}
			if ok {
				return value, true, nil
			}
		}
	}
	if s.remote == nil {
		return zero, false, nil
	}
	value, ok, err := s.remote.Get(ctx, key)
	if err != nil || !ok {
		return value, ok, err
	}
	if s.local != nil {
		// A failed L1 backfill must not fail the read, but it must not be
		// invisible either: a store that never accepts a backfill turns every
		// read into a remote round trip with no signal that it is happening.
		// ErrStaleWrite is not a degradation — it means a newer value is
		// already cached, which is the outcome we wanted.
		if err := s.local.Set(ctx, value); err != nil && !errors.Is(err, ErrStaleWrite) {
			metrics.IncCounter("cache.layered.backfill_failed.total", nil, 1)
		} else {
			s.setLocalExpiry(key, time.Now())
		}
	}
	return value, true, nil
}

func (s *LayeredStore[K, V]) Set(ctx context.Context, value V) error {
	if s == nil {
		return nil
	}
	if s.cfg.ValidateValue != nil {
		if err := s.cfg.ValidateValue(value); err != nil {
			return err
		}
	}
	key, err := s.cfg.keyOf(value)
	if err != nil {
		return err
	}
	if s.remote != nil {
		if err := s.remote.Set(ctx, value); err != nil {
			return err
		}
		current, ok, err := s.remote.Get(ctx, key)
		if err != nil {
			return err
		}
		if ok {
			value = current
		} else {
			if s.local != nil {
				_ = s.local.Delete(ctx, key)
			}
			s.clearLocalExpiry(key)
			return nil
		}
	}
	if s.local != nil {
		if err := s.local.Set(ctx, value); err != nil {
			return err
		}
		s.setLocalExpiry(key, time.Now())
	}
	return nil
}

func (s *LayeredStore[K, V]) Delete(ctx context.Context, key K) error {
	if s == nil || !s.cfg.validKey(key) {
		return nil
	}
	if s.remote != nil {
		if err := s.remote.Delete(ctx, key); err != nil {
			return err
		}
	}
	if s.local != nil {
		if err := s.local.Delete(ctx, key); err != nil {
			return err
		}
	}
	s.clearLocalExpiry(key)
	return nil
}

// localValid reports whether the L1 copy of key may be served without
// consulting the remote.
//
// A non-positive ttl means "do not serve from L1", not "serve from L1
// forever". The opposite reading is the dangerous one: a caller that leaves
// the TTL unset — which is what a missing config key yields, since
// viper.GetDuration returns 0 — would get a first-level cache that never
// revalidates, so every replica reads its own permanently stale view. Get
// already short-circuits when there is no remote, so returning false here
// degrades to "no L1 caching" rather than to "no store".
func (s *LayeredStore[K, V]) localValid(key K, now time.Time) bool {
	if s == nil {
		return false
	}
	if s.ttl <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.expiry[key]
	if !ok || now.After(exp) {
		delete(s.expiry, key)
		return false
	}
	return true
}

func (s *LayeredStore[K, V]) setLocalExpiry(key K, now time.Time) {
	if s == nil || s.ttl <= 0 {
		return
	}
	s.mu.Lock()
	s.expiry[key] = now.Add(s.ttl)
	s.mu.Unlock()
}

func (s *LayeredStore[K, V]) clearLocalExpiry(key K) {
	if s == nil || s.ttl <= 0 {
		return
	}
	s.mu.Lock()
	delete(s.expiry, key)
	s.mu.Unlock()
}
