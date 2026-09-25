package entitysync

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// 后一个会话拒绝后，全局内容版本尚未提交，但前一个客户端已应用增量。
// 重试必须恢复成可应用的全量，不能再次交付以旧版本为基线的增量。
func TestPartialTickRetryRestoresAnApplicableSubjectBaseline(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1201, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2)
	for _, sid := range []SessionID{1, 2} {
		mustSubscribe(t, manager, sid, 1201, entity.SyncProfile{})
	}
	mustFlush(t, manager)
	transport.take(1)
	transport.take(2)

	state.MarkDirty(1)
	transport.breakSession(2, ErrRetryLater)
	if err := manager.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("partial tick: %v", err)
	}
	delivered := oneFrame(t, transport, 1)
	if !state.PendingDirty() {
		t.Fatal("retry consumed the subject's dirty state")
	}
	// 下一次捕获还可能包含失败之后发生的新变化。
	state.MarkDirty(2)
	transport.breakSession(2, nil)
	mustFlush(t, manager)
	for _, sid := range []SessionID{1, 2} {
		retried := oneFrame(t, transport, sid)
		update := retried.updates[1201]
		if sid == 1 {
			if retried.wire.BaseTick != delivered.wire.Tick {
				t.Fatalf("frame base %d, client holds %d", retried.wire.BaseTick, delivered.wire.Tick)
			}
			if !update.Full && update.BaseVersion != delivered.updates[1201].Version {
				t.Fatalf("retry delta starts at %d but client holds %d", update.BaseVersion, delivered.updates[1201].Version)
			}
		}
		if update.Version != state.Version() {
			t.Fatalf("session %d version %d, committed %d", sid, update.Version, state.Version())
		}
	}
	if state.PendingDirty() || manager.Stats().SessionsLost != 0 {
		t.Fatal("successful retry must commit without losing a session")
	}
	if got := manager.Stats().FramesAdmitted; got != 5 {
		t.Fatalf("admitted frames = %d, want initial 2 + partial 1 + retry 2", got)
	}
}

// 一次 Flush 可拆成多帧。第一帧已交付、第二帧拒绝时，重试的帧时钟必须
// 接着客户端最后收到的帧继续，不能倒退到 Flush 开始前。
func TestPartialSessionRetryContinuesAfterTheLastAdmittedFrame(t *testing.T) {
	recorder := newRecordingTransport()
	calls, failAt := 0, 0
	transport := TransportFunc(func(ctx context.Context, sid SessionID, payload []byte) error {
		calls++
		if calls == failAt {
			return ErrRetryLater
		}
		return recorder.Push(ctx, sid, payload)
	})
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 2}})
	open(t, manager, 1)
	packs := 0
	for _, id := range []int64{1, 2, 3, 4} {
		if err := manager.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{1, 2} {
		mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, manager)
	oneFrame(t, recorder, 1)
	for _, id := range []int64{1, 2} {
		if err := manager.Unsubscribe(1, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{3, 4} {
		mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	}
	failAt = calls + 2
	if err := manager.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("partial session tick: %v", err)
	}
	admitted := oneFrame(t, recorder, 1)
	if len(admitted.wire.Objects) != 2 || admitted.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("expected the removal frame before the outage: %+v", admitted.wire)
	}
	failAt = 0
	mustFlush(t, manager)
	frames := recorder.take(1)
	if len(frames) == 0 {
		t.Fatal("retry did not deliver the replacements")
	}
	lastTick := admitted.wire.Tick
	created := 0
	for _, payload := range frames {
		got := decodeFrame(t, payload)
		if got.wire.BaseTick != lastTick {
			t.Fatalf("retry frame base %d, client already holds tick %d", got.wire.BaseTick, lastTick)
		}
		lastTick = got.wire.Tick
		for _, object := range got.wire.Objects {
			if object.Operation != frame.ObjectCreate {
				t.Fatalf("retry repeated an already delivered removal: %+v", object)
			}
			created++
		}
	}
	if created != 2 || manager.Stats().SessionsLost != 0 {
		t.Fatalf("created %d objects, stats %+v", created, manager.Stats())
	}
	if got := manager.Stats().FramesAdmitted; got != 3 {
		t.Fatalf("admitted frames = %d, want initial 1 + partial 1 + retry 1", got)
	}
}
