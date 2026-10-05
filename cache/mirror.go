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

func (s *ReplicaSyncer[K, V]) Start() error {
	if s == nil || s.replicator == nil {
		return nil
	}
	return s.replicator.Start()
}

func (s *ReplicaSyncer[K, V]) Stop() {
	if s != nil && s.replicator != nil {
		s.replicator.Stop()
	}
}

func (s *ReplicaSyncer[K, V]) Publish(ctx context.Context, value V) error {
	if s == nil || s.replicator == nil || s.cfg.KeyOf == nil {
		return nil
	}
	version := int64(0)
	if s.cfg.VersionOf != nil {
		version = s.cfg.VersionOf(value)
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

func (s *ReplicaSyncer[K, V]) PublishDelete(ctx context.Context, replicaKey int64, version int64) error {
	if s == nil || s.replicator == nil {
		return nil
	}
	return s.replicator.PublishDelete(ctx, replicaKey, version)
}

type replicaStore[K comparable, V any] struct {
	cfg ReplicaConfig[K, V]
}

func (s replicaStore[K, V]) ApplyReplica(ctx context.Context, env mirror.Envelope) error {
	if s.cfg.Store == nil || env.Key == 0 {
		return nil
	}
	if env.Op == mirror.OpDelete {
		if s.cfg.DeleteKeyOf == nil {
			return nil
		}
		return s.cfg.Store.Delete(ctx, s.cfg.DeleteKeyOf(env.Key))
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
	// 配置声明的身份；未配置版本提取器仍保留不带版本的接入方式。
	if s.cfg.KeyOf != nil && s.cfg.KeyOf(value) != env.Key {
		return fmt.Errorf("cache: replica payload key does not match envelope key %d", env.Key)
	}
	if s.cfg.VersionOf != nil && s.cfg.VersionOf(value) != env.Version {
		return fmt.Errorf("cache: replica payload version does not match envelope version %d", env.Version)
	}
	return s.cfg.Store.Set(ctx, value)
}
