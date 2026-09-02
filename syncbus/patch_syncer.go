package syncbus

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	stdsync "sync"
	"sync/atomic"
)

// PatchSyncerConfig describes transient patch replication over ISyncBus.
// It intentionally has no store/delete/stale semantics; long-lived state belongs
// in cache replica or entity snapshot layers.
type PatchSyncerConfig[T any] struct {
	Topic     string
	LocalSid  int32
	KeyOf     func(T) int64
	VersionOf func(T) int64
	WithKey   func(T, int64) T
	HasData   func(T) bool
	Apply     func(context.Context, T) error
}

type PatchSyncer[T any] struct {
	bus         ISyncBus
	cfg         PatchSyncerConfig[T]
	mu          stdsync.Mutex
	unsub       func()
	publisherID string
	sequence    atomic.Uint64
}

func NewPatchSyncer[T any](bus ISyncBus, cfg PatchSyncerConfig[T]) *PatchSyncer[T] {
	return &PatchSyncer[T]{bus: bus, cfg: cfg, publisherID: rand.Text()}
}

func (s *PatchSyncer[T]) Start() error {
	if s == nil {
		return fmt.Errorf("sync patch: syncer is nil")
	}
	if s.bus == nil || s.cfg.Topic == "" || s.cfg.KeyOf == nil || s.cfg.Apply == nil {
		return fmt.Errorf("sync patch: bus, topic, key function and apply function are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsub != nil {
		return nil
	}
	unsub, err := s.bus.Subscribe(s.cfg.Topic, s.handle)
	if err != nil {
		return err
	}
	s.unsub = unsub
	return nil
}

func (s *PatchSyncer[T]) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsub != nil {
		s.unsub()
	}
	s.unsub = nil
}

func (s *PatchSyncer[T]) Publish(ctx context.Context, patch T) error {
	if s == nil {
		return fmt.Errorf("sync patch: syncer is nil")
	}
	if s.bus == nil || s.cfg.Topic == "" || s.cfg.KeyOf == nil {
		return fmt.Errorf("sync patch: bus, topic and key function are required")
	}
	if s.empty(patch) {
		return nil
	}
	key := s.keyOf(patch)
	if key == 0 {
		return fmt.Errorf("sync patch: key is zero")
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	msg := &SyncMsg{
		MessageID: fmt.Sprintf("patch:%s:%d", s.publisherID, s.sequence.Add(1)),
		Topic:     s.cfg.Topic, Key: key, Version: s.versionOf(patch),
		Data: data, FromSid: s.cfg.LocalSid,
	}
	if publisher, ok := s.bus.(IContextPublisher); ok {
		return publisher.PublishContext(ctx, msg)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.bus.Publish(msg)
}

func (s *PatchSyncer[T]) handle(msg *SyncMsg) error {
	if s == nil || msg == nil || msg.Key == 0 || len(msg.Data) == 0 {
		return nil
	}
	if s.cfg.LocalSid != 0 && msg.FromSid == s.cfg.LocalSid {
		return nil
	}
	var patch T
	if err := json.Unmarshal(msg.Data, &patch); err != nil {
		return err
	}
	key := s.keyOf(patch)
	if key == 0 && s.cfg.WithKey != nil {
		patch = s.cfg.WithKey(patch, msg.Key)
		key = s.keyOf(patch)
	}
	if key != msg.Key {
		return fmt.Errorf("sync patch: key mismatch key=%d patch=%d", msg.Key, key)
	}
	if s.empty(patch) {
		return nil
	}
	return s.cfg.Apply(context.Background(), patch)
}

func (s *PatchSyncer[T]) keyOf(patch T) int64 {
	if s == nil || s.cfg.KeyOf == nil {
		return 0
	}
	return s.cfg.KeyOf(patch)
}

func (s *PatchSyncer[T]) empty(patch T) bool {
	return s != nil && s.cfg.HasData != nil && !s.cfg.HasData(patch)
}

func (s *PatchSyncer[T]) versionOf(patch T) int64 {
	if s == nil || s.cfg.VersionOf == nil {
		return 0
	}
	return s.cfg.VersionOf(patch)
}
