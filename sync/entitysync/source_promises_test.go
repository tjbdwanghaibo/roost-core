package entitysync

import (
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
	"testing"
)

func TestSourcesSelectOneProfileAndReleaseIndependently(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 5301, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	near, far := manager.NewSubscriptionSource(), manager.NewSubscriptionSource()
	bind := func(source *SubscriptionSource, p entity.SyncProfile) {
		t.Helper()
		if err := source.Subscribe(1, 5301, p); err != nil {
			t.Fatal(err)
		}
	}
	bind(far, entity.SyncProfile{Key: "far", LOD: 3})
	for range 3 {
		bind(near, entity.SyncProfile{Key: "near", LOD: 1})
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); got.updates[5301].Profile.Key != "near" || packs != 1 {
		t.Fatalf("winning profile/pack count: %+v packs=%d", got.updates, packs)
	}
	// 低优先级变化以及同来源重试都不应使生效视图重发。
	bind(far, entity.SyncProfile{Key: "farther", LOD: 4})
	bind(near, entity.SyncProfile{Key: "near", LOD: 1})
	mustFlush(t, manager)
	if len(transport.take(1)) != 0 {
		t.Fatal("unchanged active profile resent content")
	}
	// 默认 API 是第三个独立来源，释放它也不能撤销其他来源。
	mustSubscribe(t, manager, 1, 5301, entity.SyncProfile{Key: "default-source", LOD: 9})
	if err := manager.Unsubscribe(1, 5301); err != nil {
		t.Fatal(err)
	}
	if err := near.Unsubscribe(1, 5301); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); !got.updates[5301].Full || got.updates[5301].Profile.Key != "farther" {
		t.Fatal("profile fallback must resend full content")
	}
	if err := far.Unsubscribe(1, 5301); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	got := oneFrame(t, transport, 1)
	if len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatal("final source did not remove object")
	}
	if len(manager.Subscribers(5301)) != 0 {
		t.Fatal("idempotent subscribes leaked ownership")
	}
}

func TestProfilePriorityHasDeterministicTieBreakers(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	if err := manager.Register(testSubject(t, 5302, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	sources := []*SubscriptionSource{manager.NewSubscriptionSource(), manager.NewSubscriptionSource(), manager.NewSubscriptionSource()}
	profiles := []entity.SyncProfile{{Key: "b", LOD: 1, SchemaVersion: 1}, {Key: "a", LOD: 1, SchemaVersion: 2}, {Key: "a", LOD: 1, SchemaVersion: 1}}
	for i, source := range sources {
		if err := source.Subscribe(1, 5302, profiles[i]); err != nil {
			t.Fatal(err)
		}
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); got.updates[5302].Profile != profiles[2] {
		t.Fatalf("winner: %+v", got.updates[5302].Profile)
	}
	if err := sources[2].Unsubscribe(1, 5302); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); !got.updates[5302].Full || got.updates[5302].Profile != profiles[1] {
		t.Fatal("schema fallback lost")
	}
}

func TestRetirementCannotBeUndoneByReleasingOneSource(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	if err := manager.Register(testSubject(t, 5303, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	a, b := manager.NewSubscriptionSource(), manager.NewSubscriptionSource()
	if err := a.Subscribe(1, 5303, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(1, 5303, entity.SyncProfile{LOD: 2}); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	transport.take(1)
	if err := manager.Unregister(5303); err != nil {
		t.Fatal(err)
	}
	_ = a.Unsubscribe(1, 5303)
	mustFlush(t, manager)
	got := oneFrame(t, transport, 1)
	if len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove || manager.Stats().Subjects != 0 {
		t.Fatal("source release revived a retiring subject")
	}
}
