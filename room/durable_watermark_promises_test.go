package room

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/roost-core/entitysync"
)

// U-0233 · C4 · RR-20260918-02：RoomBroadcaster 自己建并私有持有一个
// SubscriptionCoordinator，而 coordinator 早就有 SetDurableWatermark——房间层却没有
// 任何入口把水位源交进去，也不在内部安装。于是走默认房间链的部署拿不到 pipelined
// 提交的外发屏障：尚未持久的内容照发。承诺：房间配置能接收水位源，订阅 / 单体 flush /
// 批量 flush 三条路径都受它约束，并在水位推进后补发。

func TestRoomBroadcasterGatesOnTheDurableWatermark(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	var durable atomic.Uint64
	room, err := NewRoomBroadcaster(91, recorder, RoomBroadcasterConfig{
		DurableWatermark: durable.Load,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = room.Close(context.Background()) })

	state := testRoomState(201, nil)
	// The subject's newest pipelined commit is 10; the deployment has made 9
	// durable, so nothing about it may go out yet.
	state.SetLastCommitLSN(10)
	durable.Store(9)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 1}
	profile := entity.SyncProfile{Key: "near", LOD: 1, SchemaVersion: 3}
	// Deferral is a recognizable retryable error, not silence: the caller has
	// to be able to tell "not yet" from "never" (the entitysync contract).
	if _, err := room.Subscribe(context.Background(), subscriber, 201, profile); !errors.Is(err, coreentitysync.ErrDurabilityDeferred) {
		t.Fatalf("subscribe below the watermark returned %v, want ErrDurabilityDeferred", err)
	}
	if frames := recorder.lastBatch(); len(frames) != 0 {
		t.Fatalf("a subscription snapshot went out below the durable watermark: %d frames", len(frames))
	}
	state.MarkFullDirty(1)
	// The two flush paths report deferral differently, and both keep the
	// subject dirty so the next tick retries it: a single-subject flush skips
	// silently (it is the tick's own path and a deferral is not an error
	// there), a batch flush surfaces it. What matters here is that neither
	// delivers.
	if err := room.FlushSubject(context.Background(), 201); err != nil {
		t.Fatalf("single-subject flush below the watermark returned %v, want a silent skip", err)
	}
	if frames := recorder.lastBatch(); len(frames) != 0 {
		t.Fatalf("a flush went out below the durable watermark: %d frames", len(frames))
	}
	if err := room.FlushDirty(context.Background()); !errors.Is(err, coreentitysync.ErrDurabilityDeferred) {
		t.Fatalf("batch flush below the watermark returned %v, want ErrDurabilityDeferred", err)
	}
	if frames := recorder.lastBatch(); len(frames) != 0 {
		t.Fatalf("a batch flush went out below the durable watermark: %d frames", len(frames))
	}

	// Once the commit is durable the subscription and its content go out.
	durable.Store(10)
	if _, err := room.Subscribe(context.Background(), subscriber, 201, profile); err != nil {
		t.Fatalf("subscribe after the watermark moved: %v", err)
	}
	if err := room.FlushDirty(context.Background()); err != nil {
		t.Fatalf("flush dirty after the watermark moved: %v", err)
	}
	if frames := recorder.lastBatch(); len(frames) == 0 {
		t.Fatal("nothing went out after the watermark reached the subject's commit")
	}
}

// A deployment with synchronous durable commits has no watermark to install;
// leaving it nil must keep the room delivering rather than gating forever.
func TestRoomWithoutAWatermarkDeliversImmediately(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	room, err := NewRoomBroadcaster(92, recorder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = room.Close(context.Background()) })
	state := testRoomState(202, nil)
	state.SetLastCommitLSN(10)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 2}
	if _, err := room.Subscribe(context.Background(), subscriber, 202, entity.SyncProfile{Key: "near", LOD: 1, SchemaVersion: 3}); err != nil {
		t.Fatal(err)
	}
	if frames := recorder.lastBatch(); len(frames) == 0 {
		t.Fatal("a room with no watermark held back its subscription snapshot")
	}
}

// The manager owns the rooms, so its configuration is where a deployment
// names the watermark once.
func TestRoomManagerPassesTheWatermarkToItsRooms(t *testing.T) {
	recorder := &recordingRoomFrameSink{}
	var durable atomic.Uint64
	manager, err := NewRoomManager(RoomManagerConfig{
		Downstream:          recorder,
		MaxRooms:            4,
		ReplicationInterval: time.Hour,
		DurableWatermark:    durable.Load,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	room, err := manager.Create(93)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(203, nil)
	state.SetLastCommitLSN(5)
	durable.Store(4)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 3}
	profile := entity.SyncProfile{Key: "near", LOD: 1, SchemaVersion: 3}
	if _, err := room.Subscribe(context.Background(), subscriber, 203, profile); !errors.Is(err, coreentitysync.ErrDurabilityDeferred) {
		t.Fatalf("a room built by the manager ignored the configured watermark: %v", err)
	}
	if frames := recorder.lastBatch(); len(frames) != 0 {
		t.Fatalf("a room built by the manager delivered below the watermark: %d frames", len(frames))
	}
	durable.Store(5)
	if _, err := room.Subscribe(context.Background(), subscriber, 203, profile); err != nil {
		t.Fatalf("subscribe after the watermark moved: %v", err)
	}
	state.MarkFullDirty(1)
	if err := room.FlushDirty(context.Background()); err != nil {
		t.Fatal(err)
	}
	if frames := recorder.lastBatch(); len(frames) == 0 {
		t.Fatal("the room never delivered after the watermark moved")
	}
}
