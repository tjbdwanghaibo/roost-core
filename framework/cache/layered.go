package cache

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// LayeredOptions 限制 L1 准入元数据；淘汰只增加回源，不删除 L1/L2 数据。
// MaxExpiryEntries 未设置时最多保留 65536 个键；本地缓存较小时可同步调低。
type LayeredOptions struct {
	MaxExpiryEntries int
}

type layeredExpiry[K comparable] struct {
	key       K
	expiresAt time.Time
}

type LayeredStore[K comparable, V any] struct {
	cfg    StoreConfig[K, V]
	local  Store[K, V]
	remote Store[K, V]
	ttl    time.Duration

	mu               sync.Mutex
	expiry           map[K]*list.Element
	order            list.List
	maxExpiryEntries int
}

func NewLayeredStore[K comparable, V any](local Store[K, V], remote Store[K, V], ttl time.Duration, cfg StoreConfig[K, V], options ...LayeredOptions) *LayeredStore[K, V] {
	limit := 65536
	for _, option := range options {
		if option.MaxExpiryEntries > 0 {
			limit = option.MaxExpiryEntries
		}
	}
	return &LayeredStore[K, V]{
		cfg:              cfg,
		local:            local,
		remote:           remote,
		ttl:              ttl,
		expiry:           make(map[K]*list.Element),
		maxExpiryEntries: limit,
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
		backfillErr := s.local.Set(ctx, value)
		// RR-20261004-02：L1 的副本只在自己的 TTL 窗口内算“已准入”。窗口外
		// （ttl≤0 时永远在窗口外）它只是旧缓存，没有否决权威的资格：不同生命周期
		// 的旧版本（删除后重建、Redis TTL 到期后晚到的写）会让它永久拒绝回填。
		// 先删掉它再回填一次；再次被拒说明是删除后才落下的新状态（较新的删除墓碑、
		// 并发 Set），交给下面 NC-14 的规则处理。
		if errors.Is(backfillErr, ErrStaleWrite) && !s.localValid(key, time.Now()) {
			if err := s.local.Delete(ctx, key); err != nil {
				metrics.IncCounter("cache.layered.backfill_failed.total", nil, 1)
				return value, true, nil
			}
			if s.ttl <= 0 {
				return value, true, nil
			}
			backfillErr = s.local.Set(ctx, value)
		}
		// RR-20261004-NC-14：准入拒绝不是L1可用性故障，不能交付被拒的值。
		// 读取已准入值；较新删除造成的miss保持miss，不续被拒回填的TTL。
		if errors.Is(backfillErr, ErrStaleWrite) || errors.Is(backfillErr, ErrConflictingWrite) {
			if errors.Is(backfillErr, ErrConflictingWrite) {
				metrics.IncCounter("cache.layered.backfill_failed.total", nil, 1)
			}
			current, held, getErr := s.local.Get(ctx, key)
			if getErr != nil {
				return zero, false, errors.Join(backfillErr, getErr)
			}
			if held {
				return current, true, nil
			}
			if errors.Is(backfillErr, ErrStaleWrite) {
				return zero, false, nil
			}
			return zero, false, backfillErr
		}
		// 普通L1故障仍允许交付L2结果，并保留失败指标。
		if backfillErr != nil {
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
			// RR-20261004-02：远端已经接受这次写，L1 以 stale 拒绝的只是它自己的
			// 旧副本（或并发写入的更新副本）。写已生效，不能报成拒绝；删掉 L1
			// 让下一次 Get 回到权威。没有远端时 L1 就是存储本身，拒绝照常返回。
			if s.remote != nil && errors.Is(err, ErrStaleWrite) {
				_ = s.local.Delete(ctx, key)
				s.clearLocalExpiry(key)
				return nil
			}
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
	var remoteErr error
	if s.remote != nil {
		remoteErr = s.remote.Delete(ctx, key)
	}
	// RR-20261004-09：远端删除报错（明确拒绝、并发方已先删、网络 / 结果未知）
	// 时也要丢掉 L1。原先直接返回，记录在 Redis 里已被删掉，本进程 L1 却在 TTL
	// 窗口内继续返回它。丢缓存总是安全的：远端没删成，下一次 Get 读回权威值。
	// 远端错误照常返回；Get / Set 的回填规则（RR-20261004-02）不变。
	var localErr error
	if s.local != nil {
		localErr = s.local.Delete(ctx, key)
	}
	s.clearLocalExpiry(key)
	if remoteErr != nil && localErr != nil {
		return errors.Join(remoteErr, localErr)
	}
	if remoteErr != nil {
		return remoteErr // 原样透传，不包一层
	}
	return localErr
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
	entry := s.expiry[key]
	if entry == nil {
		return false
	}
	if !now.Before(entry.Value.(layeredExpiry[K]).expiresAt) {
		s.removeExpiry(entry)
		return false
	}
	return true
}

func (s *LayeredStore[K, V]) setLocalExpiry(key K, now time.Time) {
	if s == nil || s.ttl <= 0 || s.remote == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 按最近一次准入排列，顺手回收队首过期项；容量上限还覆盖高基数和长 TTL。
	// 不开常驻清理 goroutine，空闲时最多保留固定容量，下一次准入继续回收。
	for first := s.order.Front(); first != nil && !now.Before(first.Value.(layeredExpiry[K]).expiresAt); first = s.order.Front() {
		s.removeExpiry(first)
	}
	value := layeredExpiry[K]{key: key, expiresAt: now.Add(s.ttl)}
	if entry := s.expiry[key]; entry != nil {
		entry.Value = value
		s.order.MoveToBack(entry)
		return
	}
	if s.order.Len() >= s.maxExpiryEntries {
		s.removeExpiry(s.order.Front())
	}
	s.expiry[key] = s.order.PushBack(value)
}

func (s *LayeredStore[K, V]) clearLocalExpiry(key K) {
	if s == nil || s.ttl <= 0 {
		return
	}
	s.mu.Lock()
	if entry := s.expiry[key]; entry != nil {
		s.removeExpiry(entry)
	}
	s.mu.Unlock()
}

// removeExpiry 只在 mu 下调用。丢失准入记录的键必须重新向权威读取。
func (s *LayeredStore[K, V]) removeExpiry(entry *list.Element) {
	delete(s.expiry, entry.Value.(layeredExpiry[K]).key)
	s.order.Remove(entry)
}
