package cache

import (
	"bytes"
	"context"
	"fmt"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

type ReplicaConfig[K comparable, V any] struct {
	Store       Store[K, V]
	Topic       string
	KeyOf       func(V) int64
	VersionOf   func(V) int64
	UpdatedAtOf func(V) int64
	DeleteKeyOf func(replicaKey int64) K
}

type ReplicaSyncer[K comparable, V any] struct {
	cfg        ReplicaConfig[K, V]
	replicator *mirror.Replicator
}

func NewReplicaSyncer[K comparable, V any](bus fsyncbus.ISyncBus, cfg ReplicaConfig[K, V]) *ReplicaSyncer[K, V] {
	s := &ReplicaSyncer[K, V]{cfg: cfg}
	s.replicator = mirror.New(bus, cfg.Topic, replicaStore[K, V]{cfg: cfg})
	return s
}

func (s *ReplicaSyncer[K, V]) validate() error {
	if s == nil || s.replicator == nil {
		return fmt.Errorf("cache: replica syncer is not initialized")
	}
	if s.cfg.Store == nil || s.cfg.KeyOf == nil || s.cfg.VersionOf == nil || s.cfg.DeleteKeyOf == nil {
		return fmt.Errorf("cache: replica store, key, version and delete-key extractors are required")
	}
	if _, ok := s.cfg.Store.(ReplicaStore[K, V]); !ok {
		return fmt.Errorf("cache: replica store must atomically apply versions and deletion watermarks")
	}
	return nil
}
func (s *ReplicaSyncer[K, V]) Start() error {
	if err := s.validate(); err != nil {
		return err
	}
	return s.replicator.Start()
}

// Stop 退订并在 ctx 内等在途的 Store 写入返回（三步停机，经 Replicator.StopWithContext）：超时返回 ctx
// 错误，重试再等同一批；返回 nil 之后 Store 不会再被复制消息调用，调用方可以释放它。
func (s *ReplicaSyncer[K, V]) Stop(ctx context.Context) error {
	if s == nil || s.replicator == nil {
		return nil
	}
	return s.replicator.StopWithContext(ctx)
}

func (s *ReplicaSyncer[K, V]) Publish(ctx context.Context, value V) error {
	if err := s.validate(); err != nil {
		return err
	}
	version := s.cfg.VersionOf(value)
	if version <= 0 {
		return fmt.Errorf("cache: replica version must be positive")
	}
	updatedAt := int64(0)
	if s.cfg.UpdatedAtOf != nil {
		updatedAt = s.cfg.UpdatedAtOf(value)
	}
	if updatedAt == 0 {
		updatedAt = time.Now().UnixMilli()
	}
	raw, err := mirror.MarshalPayload(value)
	if err != nil {
		return err
	}
	return s.replicator.Publish(ctx, mirror.Envelope{
		Key:       s.cfg.KeyOf(value),
		Version:   version,
		Op:        mirror.OpUpsert,
		Payload:   raw,
		UpdatedAt: updatedAt,
	})
}

func (s *ReplicaSyncer[K, V]) PublishDelete(ctx context.Context, replicaKey, version int64) error {
	if err := s.validate(); err != nil {
		return err
	}
	if version <= 0 {
		return ErrReplicaDeleteVersion
	}
	return s.replicator.PublishDelete(ctx, replicaKey, version)
}

type replicaStore[K comparable, V any] struct {
	cfg ReplicaConfig[K, V]
}

func (s replicaStore[K, V]) ApplyReplica(ctx context.Context, env mirror.Envelope) error {
	if env.Key == 0 || env.Version <= 0 {
		return fmt.Errorf("cache: replica key and positive version are required")
	}
	store, ok := s.cfg.Store.(ReplicaStore[K, V])
	if !ok {
		return fmt.Errorf("cache: atomic replica store is required")
	}
	if env.Op == mirror.OpDelete {
		var zero V
		return store.ApplyReplicaValue(ctx, s.cfg.DeleteKeyOf(env.Key), zero, env.Version, true)
	}
	// 含身份的更新不能是 null；先拒绝，避免把 nil 指针交给身份提取器。
	if (s.cfg.KeyOf != nil || s.cfg.VersionOf != nil) && bytes.Equal(bytes.TrimSpace(env.Payload), []byte("null")) {
		return fmt.Errorf("cache: replica payload is null")
	}
	value, err := mirror.UnmarshalPayload[V](env)
	if err != nil {
		return err
	}
	// RR-20261005-NC-33：两层信封一致不代表业务对象一致，写入前绑定
	// 配置声明的身份；版本和删除身份提取器由 Start 强制要求。
	if s.cfg.KeyOf != nil && s.cfg.KeyOf(value) != env.Key {
		return fmt.Errorf("cache: replica payload key does not match envelope key %d", env.Key)
	}
	if s.cfg.VersionOf != nil && s.cfg.VersionOf(value) != env.Version {
		return fmt.Errorf("cache: replica payload version does not match envelope version %d", env.Version)
	}
	return store.ApplyReplicaValue(ctx, s.cfg.DeleteKeyOf(env.Key), value, env.Version, false)
}
