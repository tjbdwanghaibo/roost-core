package cache

import (
	"context"
	"errors"
	"reflect"
	"sync"
)

// ReplicaStore 必须将版本比较、值和删除水位作为一个原子操作。
// 持久实现必须一起落地；不能用分离的 Get/Set/Delete 假装满足此契约。
type ReplicaStore[K comparable, V any] interface {
	Store[K, V]
	ApplyReplicaValue(context.Context, K, V, int64, bool) error
}

var (
	ErrReplicaCapacity      = errors.New("cache: replica key capacity reached")
	ErrReplicaDeleteVersion = errors.New("cache: replica deletion requires a positive version")
)

type replicaValue[V any] struct {
	value   V
	version int64
	deleted bool
}

// ReplicaLocalStore 保留值和删除水位直到整个缓存被销毁。删除水位不能被 LRU
// 或 TTL 淘汰，否则旧投递会复活数据。maxKeys <= 0 使用 10000，容量满时拒绝新键。
// 它是进程内读副本；重建时从完整快照/日志重新建立值和删除水位。
type ReplicaLocalStore[K comparable, V any] struct {
	mu        sync.RWMutex
	cfg       StoreConfig[K, V]
	versionOf func(V) int64
	maxKeys   int
	items     map[K]replicaValue[V]
}

func NewReplicaLocalStore[K comparable, V any](cfg StoreConfig[K, V], versionOf func(V) int64, maxKeys int) *ReplicaLocalStore[K, V] {
	if maxKeys <= 0 {
		maxKeys = 10000
	}
	return &ReplicaLocalStore[K, V]{cfg: cfg, versionOf: versionOf, maxKeys: maxKeys, items: make(map[K]replicaValue[V])}
}
func (s *ReplicaLocalStore[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	var zero V
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.items[key]
	if !ok || entry.deleted {
		return zero, false, nil
	}
	return entry.value, true, nil
}
func (s *ReplicaLocalStore[K, V]) Set(ctx context.Context, value V) error {
	if s.versionOf == nil {
		return errors.New("cache: replica version extractor is required")
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
	return s.ApplyReplicaValue(ctx, key, value, s.versionOf(value), false)
}
func (s *ReplicaLocalStore[K, V]) Delete(context.Context, K) error { return ErrReplicaDeleteVersion }
func (s *ReplicaLocalStore[K, V]) ApplyReplicaValue(ctx context.Context, key K, value V, version int64, deleted bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if version <= 0 {
		return errors.New("cache: replica version must be positive")
	}
	if !s.cfg.validKey(key) {
		return ErrInvalidKey
	}
	if !deleted {
		if s.cfg.ValidateValue != nil {
			if err := s.cfg.ValidateValue(value); err != nil {
				return err
			}
		}
		actual, err := s.cfg.keyOf(value)
		if err != nil {
			return err
		}
		if actual != key {
			return ErrInvalidKey
		}
		if s.versionOf == nil || s.versionOf(value) != version {
			return errors.New("cache: replica value version mismatch")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.items[key]
	if exists {
		if version < old.version || (version == old.version && old.deleted && !deleted) {
			return ErrStaleWrite
		}
		if version == old.version && old.deleted == deleted {
			if deleted || reflect.DeepEqual(old.value, value) {
				return nil
			}
			return ErrConflictingWrite
		}
	} else if len(s.items) >= s.maxKeys {
		return ErrReplicaCapacity
	}
	if deleted {
		var zero V
		value = zero
	}
	s.items[key] = replicaValue[V]{value: value, version: version, deleted: deleted}
	return nil
}
