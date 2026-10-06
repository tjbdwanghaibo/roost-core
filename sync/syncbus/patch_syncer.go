package syncbus

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	stdsync "sync"
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
	bus ISyncBus
	cfg PatchSyncerConfig[T]
	mu  stdsync.Mutex
	// subs 是还没确认排空的订阅（最后一个在 started 时是当前订阅）；排空由传输的 Unsubscribe 负责（A3 ②）。
	subs    []*Subscription
	started bool
	ids     *DeliveryIDs
}

func NewPatchSyncer[T any](bus ISyncBus, cfg PatchSyncerConfig[T]) *PatchSyncer[T] {
	return &PatchSyncer[T]{bus: bus, cfg: cfg, ids: NewDeliveryIDs("patch")}
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
	if s.started {
		return nil
	}
	sub, err := s.bus.Subscribe(s.cfg.Topic, s.handle)
	if err != nil {
		return err
	}
	s.subs = append(s.subs, sub)
	s.started = true
	return nil
}

// Stop 退订并在 ctx 内等在途的 Apply 返回（三步停机）：超时返回 ctx 错误，重试再等同一批；返回 nil
// 之后 Apply 不会再被调用。停止之后可以再 Start。
func (s *PatchSyncer[T]) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.started = false
	pending := slices.Clone(s.subs)
	s.mu.Unlock()
	// 每个订阅都要发起退订：ctx 到期后剩下的 Unsubscribe 只发起、立即返回 ctx 错误。
	var firstErr error
	for _, sub := range pending {
		if err := sub.Unsubscribe(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	s.mu.Lock()
	s.subs = slices.DeleteFunc(s.subs, func(sub *Subscription) bool { return slices.Contains(pending, sub) })
	s.mu.Unlock()
	return nil
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
		MessageID: s.ids.Next(),
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

func (s *PatchSyncer[T]) handle(ctx context.Context, msg *SyncMsg) error {
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
	return s.cfg.Apply(ctx, patch)
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
