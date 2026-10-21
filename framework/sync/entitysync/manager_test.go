// Manager 交付契约与包内共用夹具：每会话先全量后增量、失败会话单独关闭、持久水位门、profile 切换、帧上限、编码缓存与工作区所有权。

package entitysync

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// ARCH-10 · M-13：实体同步是一个机制。subject 持有自己的订阅者；一个 Manager 拥有
// 全部 subject 与会话，每 tick 给每个会话**一帧**；room / AOI 只是决定谁订谁的政策。
// 下面每条都是一个会话层面的承诺：它自己收到了什么、别人的失败与它无关、订阅撤掉
// 不以"通知到它"为前提。

// recordingTransport is the wire end a test can watch and break.
type recordingTransport struct {
	mu     sync.Mutex
	frames map[SessionID][][]byte
	fail   map[SessionID]error
	retry  error
	opened map[SessionID]int
	closed map[SessionID]int
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{frames: make(map[SessionID][][]byte), fail: make(map[SessionID]error), opened: make(map[SessionID]int), closed: make(map[SessionID]int)}
}

func (t *recordingTransport) Push(_ context.Context, session SessionID, frame []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.retry != nil {
		return t.retry
	}
	if err := t.fail[session]; err != nil {
		return err
	}
	t.frames[session] = append(t.frames[session], append([]byte(nil), frame...))
	return nil
}

func (t *recordingTransport) SessionOpened(session SessionID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.opened[session]++
	return nil
}

func (t *recordingTransport) SessionClosed(session SessionID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed[session]++
}

func (t *recordingTransport) take(session SessionID) [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	frames := t.frames[session]
	delete(t.frames, session)
	return frames
}

func (t *recordingTransport) breakSession(session SessionID, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fail[session] = err
}

func (t *recordingTransport) setRetry(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.retry = err
}

// decoded is one frame as a client sees it: per subject, the operation and
// the update it carried.
type decoded struct {
	wire    frame.Frame
	objects map[int64]frame.ObjectOperation
	updates map[int64]entity.SubjectSyncUpdate
}

func decodeFrame(t *testing.T, data []byte) decoded {
	t.Helper()
	got, err := DecodeFrame(data, frame.DefaultLimits())
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	out := decoded{wire: got, objects: make(map[int64]frame.ObjectOperation), updates: make(map[int64]entity.SubjectSyncUpdate)}
	refs := make(map[frame.ObjectRef]int64)
	for _, object := range got.Objects {
		for _, component := range object.Components {
			update, err := DecodeSubjectUpdate(component.Data, 0)
			if err != nil {
				t.Fatalf("decode subject update: %v", err)
			}
			out.objects[update.SubjectID] = object.Operation
			out.updates[update.SubjectID] = update
			refs[object.Ref] = update.SubjectID
		}
		if object.Operation == frame.ObjectRemove {
			// A remove names only the ref; the test records it under the ref's id.
			out.objects[-int64(object.Ref.ID)] = frame.ObjectRemove
		}
	}
	return out
}

func testSubject(t *testing.T, id int64, packs *int) *entity.SubjectSyncState {
	t.Helper()
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, Namespace: "test",
		Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				*packs++
				return entity.TakeFrozenSyncPayload(1, []byte("snapshot:"+profile.Key)), nil
			},
			Delta: func(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
				*packs++
				return entity.TakeFrozenSyncPayload(1, []byte("delta:"+profile.Key)), nil
			},
		},
	})
}

func newTestManager(t *testing.T, transport Transport, config ManagerConfig) *Manager {
	t.Helper()
	config.Transport = transport
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return manager
}

func open(t *testing.T, manager *Manager, sessions ...SessionID) {
	t.Helper()
	for _, session := range sessions {
		if err := manager.OpenSession(session); err != nil {
			t.Fatalf("open session %d: %v", session, err)
		}
	}
}

func mustSubscribe(t *testing.T, manager *Manager, session SessionID, subject int64, profile entity.SyncProfile) {
	t.Helper()
	if err := manager.Subscribe(session, subject, profile); err != nil {
		t.Fatalf("subscribe %d → %d: %v", session, subject, err)
	}
}

func mustFlush(t *testing.T, manager *Manager) {
	t.Helper()
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func oneFrame(t *testing.T, transport *recordingTransport, session SessionID) decoded {
	t.Helper()
	frames := transport.take(session)
	if len(frames) != 1 {
		t.Fatalf("session %d received %d frame(s), want 1", session, len(frames))
	}
	return decodeFrame(t, frames[0])
}

// 订阅之后第一帧是快照（FrameFull + ObjectCreate），之后每次变化是一帧 delta；
// 两个同 profile 的会话共享一次 pack。
func TestEachSessionGetsASnapshotThenDeltasAndOnePackServesThemAll(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1001, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2)
	mustSubscribe(t, manager, 1, 1001, entity.SyncProfile{})
	mustSubscribe(t, manager, 2, 1001, entity.SyncProfile{})
	mustFlush(t, manager)
	for _, session := range []SessionID{1, 2} {
		got := oneFrame(t, transport, session)
		if got.wire.Kind != frame.Full || got.objects[1001] != frame.ObjectCreate || !got.updates[1001].Full {
			t.Fatalf("session %d first frame is not a snapshot create: kind=%v objects=%v", session, got.wire.Kind, got.objects)
		}
	}
	if packs != 1 {
		t.Fatalf("two sessions on one profile cost %d pack(s), want 1", packs)
	}

	packs = 0
	state.MarkDirty(0x4)
	mustFlush(t, manager)
	for _, session := range []SessionID{1, 2} {
		got := oneFrame(t, transport, session)
		update := got.updates[1001]
		if got.wire.Kind != frame.Delta || got.wire.BaseTick != 1 || got.objects[1001] != frame.ObjectUpdate || update.Full || update.Mask != 0x4 {
			t.Fatalf("session %d second frame is not a delta from the snapshot: kind=%v base=%d objects=%v update=%+v", session, got.wire.Kind, got.wire.BaseTick, got.objects, update)
		}
		// The snapshot carried the subject's version at the time (0: nothing
		// had been committed yet); the delta advances from exactly there.
		if update.BaseVersion != 0 || update.Version != 1 {
			t.Fatalf("delta versions %d→%d, want 0→1", update.BaseVersion, update.Version)
		}
	}
	if packs != 1 {
		t.Fatalf("the delta cost %d pack(s), want 1", packs)
	}
	if state.PendingDirty() {
		t.Fatal("the subject is still dirty after its delta was admitted")
	}

	// Flush 频率不是发送频率：内容和订阅都没变化时，不打包也不发空帧。
	packs = 0
	for range 20 {
		mustFlush(t, manager)
	}
	if packs != 0 {
		t.Fatalf("unchanged subject cost %d pack(s), want 0", packs)
	}
	for _, session := range []SessionID{1, 2} {
		if frames := transport.take(session); len(frames) != 0 {
			t.Fatalf("unchanged session %d received %d frame(s), want 0", session, len(frames))
		}
	}
}

// 一个会话推不到只关它自己：它被关闭并告知政策，别人这一帧照常。
func TestAFailingSessionIsClosedAloneAndTheOthersStillReceive(t *testing.T) {
	transport := newRecordingTransport()
	var lost []SessionID
	manager := newTestManager(t, transport, ManagerConfig{SessionLost: func(session SessionID, _ error) { lost = append(lost, session) }})
	packs := 0
	state := testSubject(t, 1002, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2, 3)
	for _, session := range []SessionID{1, 2, 3} {
		mustSubscribe(t, manager, session, 1002, entity.SyncProfile{})
	}
	mustFlush(t, manager)
	for _, session := range []SessionID{1, 2, 3} {
		transport.take(session)
	}

	transport.breakSession(2, errors.New("player tcp: session not found"))
	state.MarkDirty(1)
	mustFlush(t, manager)
	for _, session := range []SessionID{1, 3} {
		got := oneFrame(t, transport, session)
		if got.objects[1002] != frame.ObjectUpdate {
			t.Fatalf("session %d did not receive the delta after another session failed: %v", session, got.objects)
		}
	}
	if got := manager.Subscribers(1002); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("subscribers after a session failed: %v, want [1 3]", got)
	}
	if len(lost) != 1 || lost[0] != 2 {
		t.Fatalf("SessionLost reported %v, want [2]", lost)
	}
	if manager.Stats().SessionsLost != 1 || manager.Stats().Sessions != 2 {
		t.Fatalf("stats after a lost session: %+v", manager.Stats())
	}
	if state.PendingDirty() {
		t.Fatal("a lost session kept the subject dirty; the others' delivery should have committed it")
	}
}

// 传输层整体不可用时谁都不怪：tick 作废，脏状态与订阅原样保留，下一 tick 重来。
func TestRetryLaterAbandonsTheTickWithoutBlamingAnySession(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1003, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2)
	mustSubscribe(t, manager, 1, 1003, entity.SyncProfile{})
	mustSubscribe(t, manager, 2, 1003, entity.SyncProfile{})
	transport.setRetry(ErrRetryLater)
	if err := manager.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("flush with the transport down: %v, want ErrRetryLater", err)
	}
	if got := manager.Subscribers(1003); len(got) != 2 {
		t.Fatalf("a transport outage dropped subscriptions: %v", got)
	}
	if manager.Stats().Sessions != 2 || manager.Stats().SessionsLost != 0 {
		t.Fatalf("a transport outage closed sessions: %+v", manager.Stats())
	}
	transport.setRetry(nil)
	mustFlush(t, manager)
	for _, session := range []SessionID{1, 2} {
		if got := oneFrame(t, transport, session); got.objects[1003] != frame.ObjectCreate {
			t.Fatalf("session %d did not get its snapshot once the transport came back: %v", session, got.objects)
		}
	}
}

// 退订与退役都是状态变更：持有对象的会话下一帧收到 remove，从未收到过的什么都不欠；
// 最后一个 remove 发出后 subject 被遗忘，可以重新注册。
func TestUnsubscribeAndUnregisterOweARemoveOnlyToWhoHoldsTheObject(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1004, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2)
	mustSubscribe(t, manager, 1, 1004, entity.SyncProfile{})
	mustFlush(t, manager)
	transport.take(1)
	// Session 2 subscribes and unsubscribes before any tick: it holds nothing.
	mustSubscribe(t, manager, 2, 1004, entity.SyncProfile{})
	if err := manager.Unsubscribe(2, 1004); err != nil {
		t.Fatal(err)
	}
	// Session 1 holds the object: unsubscribing owes it a remove.
	if err := manager.Unsubscribe(1, 1004); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if frames := transport.take(2); len(frames) != 0 {
		t.Fatalf("a session that never received the object was sent %d frame(s)", len(frames))
	}
	got := oneFrame(t, transport, 1)
	if len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("session 1 did not receive an ObjectRemove: %+v", got.wire.Objects)
	}
	if got := manager.Subscribers(1004); len(got) != 0 {
		t.Fatalf("subscribers remain after unsubscribe: %v", got)
	}

	// Retirement: subscribe again, then Unregister → remove, then the subject is gone.
	mustSubscribe(t, manager, 1, 1004, entity.SyncProfile{})
	mustFlush(t, manager)
	transport.take(1)
	if err := manager.Unregister(1004); err != nil {
		t.Fatal(err)
	}
	if err := manager.Subscribe(2, 1004, entity.SyncProfile{}); !errors.Is(err, ErrSubjectRetiring) {
		t.Fatalf("subscribe to a retiring subject: %v, want ErrSubjectRetiring", err)
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("retirement did not tell the holder the object is gone: %+v", got.wire.Objects)
	}
	if manager.Stats().Subjects != 0 {
		t.Fatalf("the retired subject is still registered: %+v", manager.Stats())
	}
	if err := manager.Register(state); err != nil {
		t.Fatalf("re-register after retirement: %v", err)
	}
}

// 会话关闭：订阅就地消失，没有任何帧；它之前占的 subject 也不因此挂起。
func TestCloseSessionForgetsItsSubscriptionsWithoutAFrame(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1005, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2)
	mustSubscribe(t, manager, 1, 1005, entity.SyncProfile{})
	mustSubscribe(t, manager, 2, 1005, entity.SyncProfile{})
	mustFlush(t, manager)
	transport.take(1)
	transport.take(2)
	manager.CloseSession(2)
	if got := manager.Subscribers(1005); len(got) != 1 || got[0] != 1 {
		t.Fatalf("subscribers after CloseSession: %v, want [1]", got)
	}
	if transport.closed[2] != 1 {
		t.Fatalf("the transport was not told the session closed: %v", transport.closed)
	}
	state.MarkDirty(1)
	mustFlush(t, manager)
	if frames := transport.take(2); len(frames) != 0 {
		t.Fatalf("a closed session was sent %d frame(s)", len(frames))
	}
	if got := oneFrame(t, transport, 1); got.objects[1005] != frame.ObjectUpdate {
		t.Fatalf("the remaining session did not get the delta: %v", got.objects)
	}
}

// 持久化门槛挡的是整个 subject：快照和 delta 一起等，水位过了才一起走。
func TestTheDurabilityGateHoldsTheWholeSubjectUntilTheWatermarkReachesIt(t *testing.T) {
	transport := newRecordingTransport()
	var watermark uint64
	manager := newTestManager(t, transport, ManagerConfig{DurableWatermark: func() uint64 { return watermark }})
	packs := 0
	state := testSubject(t, 1006, &packs)
	state.SetLastCommitLSN(10)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 1006, entity.SyncProfile{})
	state.MarkDirty(1)
	watermark = 9
	mustFlush(t, manager)
	if frames := transport.take(1); len(frames) != 0 {
		t.Fatalf("content above the durable watermark was sent: %d frame(s)", len(frames))
	}
	if !state.PendingDirty() || manager.Stats().Pending != 1 || manager.Stats().DurabilityDeferred != 1 {
		t.Fatalf("a deferred subject lost its dirty state or its place in the queue: dirty=%v stats=%+v", state.PendingDirty(), manager.Stats())
	}
	watermark = 10
	mustFlush(t, manager)
	got := oneFrame(t, transport, 1)
	if got.objects[1006] != frame.ObjectCreate || !got.updates[1006].Full || got.updates[1006].Version != 1 {
		t.Fatalf("once durable, the session did not get a snapshot at the committed version: %v %+v", got.objects, got.updates[1006])
	}
	if state.PendingDirty() {
		t.Fatal("the subject stayed dirty after its content was delivered")
	}
}

// 换 profile 等于重新拿快照：同一个对象上的 ObjectUpdate，但内容是 Full。
func TestChangingTheProfileResendsAFullSnapshotOnTheSameObject(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1007, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 1007, entity.SyncProfile{Key: "near", LOD: 1})
	mustFlush(t, manager)
	first := oneFrame(t, transport, 1)
	mustSubscribe(t, manager, 1, 1007, entity.SyncProfile{Key: "far", LOD: 2})
	mustFlush(t, manager)
	second := oneFrame(t, transport, 1)
	if second.objects[1007] != frame.ObjectUpdate || !second.updates[1007].Full || second.updates[1007].Profile.Key != "far" {
		t.Fatalf("profile change did not resend a full snapshot under the new profile: %v %+v", second.objects, second.updates[1007])
	}
	if first.wire.Objects[0].Ref != second.wire.Objects[0].Ref {
		t.Fatalf("the object changed reference across a profile change: %v → %v", first.wire.Objects[0].Ref, second.wire.Objects[0].Ref)
	}
}

// 一个会话一 tick 一帧，帧里装它订阅的所有变化；超过对象上限才分成多帧。
func TestOneFramePerSessionPerTickSplitOnlyAtTheObjectLimit(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 2}})
	packs := 0
	states := []*entity.SubjectSyncState{testSubject(t, 2001, &packs), testSubject(t, 2002, &packs), testSubject(t, 2003, &packs)}
	open(t, manager, 1)
	for _, state := range states[:2] {
		if err := manager.Register(state); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, manager, 1, state.SubjectID(), entity.SyncProfile{})
	}
	mustFlush(t, manager)
	got := oneFrame(t, transport, 1)
	if len(got.wire.Objects) != 2 {
		t.Fatalf("two pending subjects did not share one frame: %+v", got.wire.Objects)
	}
	if err := manager.Register(states[2]); err != nil {
		t.Fatal(err)
	}
	if err := manager.Subscribe(1, 2003, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	// The third object does not fit the session's object budget (MaxObjects
	// is also how many subjects a session may hold): the session is closed
	// rather than sent a frame it could not apply.
	if manager.Stats().Sessions != 0 || manager.Stats().SessionsLost != 1 {
		t.Fatalf("a session over its object budget was not closed: %+v", manager.Stats())
	}
}

// held 的会话可以订阅但收不到帧；Ready 之后第一帧才是快照。这就是"会话 ready 再开始"：
// 客户端在登录应答之后才装解码器，快照不能抢在它前面。
func TestAHeldSessionReceivesNothingUntilItIsReady(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 3001, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	if err := manager.OpenHeldSession(1); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, manager, 1, 3001, entity.SyncProfile{})
	state.MarkDirty(1)
	mustFlush(t, manager)
	mustFlush(t, manager)
	if frames := transport.take(1); len(frames) != 0 {
		t.Fatalf("a held session received %d frame(s)", len(frames))
	}
	if manager.Stats().HeldSessions != 1 || manager.Stats().Pending != 0 {
		t.Fatalf("stats while held: %+v (a held session must not keep the subject pending every tick)", manager.Stats())
	}
	if err := manager.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	got := oneFrame(t, transport, 1)
	if got.wire.Kind != frame.Full || got.objects[3001] != frame.ObjectCreate || !got.updates[3001].Full {
		t.Fatalf("the first frame after ready is not a snapshot: %v %+v", got.wire.Kind, got.objects)
	}
}

// Hold 一个已经在收帧的会话 = 让它重新开始：新 epoch、全部重发快照，中间什么都不发。
func TestHoldingALiveSessionStartsItOverInANewEpoch(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 3002, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 3002, entity.SyncProfile{})
	mustFlush(t, manager)
	first := oneFrame(t, transport, 1)
	state.MarkDirty(1)
	mustFlush(t, manager)
	transport.take(1)

	if err := manager.HoldSession(1); err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(2)
	mustFlush(t, manager)
	if frames := transport.take(1); len(frames) != 0 {
		t.Fatalf("a held session received %d frame(s)", len(frames))
	}
	if err := manager.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	again := oneFrame(t, transport, 1)
	if again.wire.Kind != frame.Full || again.wire.Epoch != first.wire.Epoch+1 || again.objects[3002] != frame.ObjectCreate {
		t.Fatalf("after hold+ready the session did not start over: kind=%v epoch=%d (was %d) objects=%v", again.wire.Kind, again.wire.Epoch, first.wire.Epoch, again.objects)
	}
	if state.PendingDirty() {
		t.Fatal("the subject stayed dirty after the resent snapshot")
	}
}

func TestPolicyFailureRetainsCauseAndCountsOnce(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{})
	cause := errors.New("policy admission rejected")
	cancel := m.RegisterPolicy(PolicyHooks{Apply: func() error { return cause }})
	if err := m.Flush(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("lost returned cause: %v", err)
	}
	if !errors.Is(m.LastError(), cause) || m.Counters().FlushFailures != 1 {
		t.Fatalf("lost policy failure: count=%d last=%v", m.Counters().FlushFailures, m.LastError())
	}
	cancel()
	mustFlush(t, m)
	if m.Counters().FlushFailures != 1 || !errors.Is(m.LastError(), cause) {
		t.Fatal("success erased history or counted again")
	}
}

// 在部分准入后取消，等同于运行循环 Stop 遇到尚在执行的 Push。
// 计数保留取消事实，但下一轮必须能补齐，不能吞掉已交付的前缀。
func TestCancelledFlushRetainsAdmissionCauseAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRecordingTransport()
	calls := 0
	transport := TransportFunc(func(ctx context.Context, sid SessionID, raw []byte) error {
		calls++
		if calls == 2 {
			cancel()
			return ctx.Err()
		}
		return r.Push(ctx, sid, raw)
	})
	m := newTestManager(t, transport, ManagerConfig{})
	open(t, m, 1, 2)
	packs := 0
	state := testSubject(t, 1, &packs)
	if err := m.Register(state); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []SessionID{1, 2} {
		mustSubscribe(t, m, sid, 1, entity.SyncProfile{})
	}
	err := m.Flush(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(m.LastError(), context.Canceled) || m.Counters().FlushFailures != 1 {
		t.Fatalf("cancel evidence lost: %v / %v", err, m.LastError())
	}
	oneFrame(t, r, 1)
	mustFlush(t, m)
	for _, sid := range []SessionID{1, 2} {
		if f := oneFrame(t, r, sid); !f.updates[1].Full {
			t.Fatalf("retry needs full for %d", sid)
		}
	}
	if m.Counters().FlushFailures != 1 || m.Stats().Pending != 0 {
		t.Fatal("failed cancellation recovery")
	}
}

func TestSessionReferenceCopiesAreIsolatedOnFirstWrite(t *testing.T) {
	original := newSession(1)
	ref, err := original.allocate(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := original.clone()
	changed := readOnly.clone()
	changed.release(100)
	replacement, err := changed.allocate(200, 10)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID != ref.ID || replacement.Generation == ref.Generation {
		t.Fatal("reference not recycled with new generation")
	}
	for _, held := range []*session{original, readOnly} {
		if held.objects[100] != ref || len(held.objects) != 1 || held.generations[ref.ID] != ref.Generation {
			t.Fatal("later frame rewrote an earlier baseline")
		}
	}
	if readOnly.referencesOwned {
		t.Fatal("read-only clone copied references")
	}
}

func TestExplicitProfilePriorityAndConfigurationOwnership(t *testing.T) {
	sink := newRecordingTransport()
	public, owner := entity.SyncProfile{Key: "a-public"}, entity.SyncProfile{Key: "z-owner", LOD: 9}
	priorities := map[entity.SyncProfile]int{owner: -10}
	m := newTestManager(t, sink, ManagerConfig{ProfilePriorities: priorities})
	priorities[owner] = 100
	packs := 0
	if err := m.Register(testSubject(t, 50, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 50, public)
	source := m.NewSubscriptionSource()
	if err := source.Subscribe(1, 50, owner); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if got := oneFrame(t, sink, 1).updates[50]; got.Profile != owner.Normalize() {
		t.Fatalf("priority ignored: %+v", got)
	}
	if err := source.Unsubscribe(1, 50); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if got := oneFrame(t, sink, 1).updates[50]; got.Profile != public.Normalize() || !got.Full {
		t.Fatalf("fallback needs a full view: %+v", got)
	}
	mustFlush(t, m)
	stats := m.Stats()
	if stats.FlushCalls != 3 || stats.EmptyFlushes != 1 || stats.CreatesAdmitted != 1 || stats.UpdatesAdmitted != 1 || stats.SnapshotsCaptured != 2 {
		t.Fatalf("incorrect counters: %+v", stats)
	}
}

func TestUnobservedCaptureStillRespectsDurabilityWatermark(t *testing.T) {
	packs := 0
	s := testSubject(t, 60, &packs)
	s.MarkDirty(1)
	s.SetLastCommitLSN(10)
	watermark := uint64(9)
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{DurableWatermark: func() uint64 { return watermark }})
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if !s.PendingDirty() || m.Stats().DurabilityDeferred != 1 || packs != 0 {
		t.Fatal("unobserved capture bypassed watermark or packed unused view")
	}
	watermark = 10
	mustFlush(t, m)
	if s.PendingDirty() {
		t.Fatal("dirty state not cleared after watermark advanced")
	}
}

func TestHiddenFieldDeltaStillAdvancesTheClientVersion(t *testing.T) {
	views, err := entity.NewSyncViewSet(entity.SyncView{Fields: 1})
	if err != nil {
		t.Fatal(err)
	}
	packer, err := entity.NewMaskedSyncPacker(views, 1, func(mask uint64) ([]byte, error) { return []byte{byte(mask)}, nil })
	if err != nil {
		t.Fatal(err)
	}
	s := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 61, Packer: packer})
	sink := newRecordingTransport()
	m := newTestManager(t, sink, ManagerConfig{})
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 61, entity.SyncProfile{})
	mustFlush(t, m)
	oneFrame(t, sink, 1)
	s.MarkDirty(2)
	mustFlush(t, m)
	hidden := oneFrame(t, sink, 1).updates[61]
	if !hidden.Payload.Empty() || hidden.BaseVersion != 0 || hidden.Version != 1 {
		t.Fatalf("hidden delta lost version: %+v", hidden)
	}
	s.MarkDirty(1)
	mustFlush(t, m)
	visible := oneFrame(t, sink, 1).updates[61]
	if visible.Payload.Empty() || visible.BaseVersion != 1 || visible.Version != 2 {
		t.Fatalf("visible delta lost baseline: %+v", visible)
	}
}

func TestTickEncodingSharesOnlyIdenticalCapturedContent(t *testing.T) {

	update := entity.SubjectSyncUpdate{SubjectID: 6101, Namespace: "cache", Profile: entity.SyncProfile{Key: "near"}, Full: true, Version: 2, Payload: entity.CopyFrozenSyncPayload(1, []byte("full"))}
	var buffer []capturedUpdate
	captured := captureUpdates(&buffer, []entity.SubjectSyncUpdate{update})
	shared, ok := updateFor(captured, update.Profile)
	if !ok {
		t.Fatal("missing capture")
	}
	first, err := shared.encode(0)
	if err != nil {
		t.Fatal(err)
	}
	same, ok := updateFor(captured, update.Profile)
	if !ok || same != shared {
		t.Fatal("same profile did not share its capture")
	}
	again, err := same.encode(0)
	if err != nil {
		t.Fatal(err)
	}
	if &first[0] != &again[0] {
		t.Fatal("identical captured content was encoded twice")
	}
	for _, variant := range []string{"delta", "profile", "subject"} {
		next := update
		switch variant {
		case "delta":
			next.Full = false
			next.BaseVersion = 1
			next.Payload = entity.CopyFrozenSyncPayload(1, []byte("delta"))
		case "profile":
			next.Profile = entity.SyncProfile{Key: "far", LOD: 2}
		case "subject":
			next.SubjectID++
		}
		want, err := EncodeSubjectUpdate(next, 0)
		if err != nil {
			t.Fatal(err)
		}
		separate := captureUpdates(&buffer, []entity.SubjectSyncUpdate{next})
		got, err := separate[0].encode(0)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) || bytes.Equal(got, first) {
			t.Fatalf("%s reused a different capture", variant)
		}
	}
	// 下一个 tick 即使键相同，也必须用新的捕获内容编码。
	update.Version++
	update.Payload = entity.CopyFrozenSyncPayload(1, []byte("new tick"))
	var nextBuffer []capturedUpdate
	nextTick := captureUpdates(&nextBuffer, []entity.SubjectSyncUpdate{update})
	later, err := nextTick[0].encode(0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(later, first) {
		t.Fatal("cache escaped its tick")
	}
}

func TestSharedEncodingKeepsSnapshotsDeltasAndProfilesSeparate(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, TransportFunc(func(ctx context.Context, id SessionID, data []byte) error {
		err := transport.Push(ctx, id, data)
		// 接入层可复用已收到的独立帧，不得破坏后续会话共享的组件缓存。
		clear(data)
		return err
	}), ManagerConfig{})
	packs := 0
	state := testSubject(t, 6102, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2, 3, 4)
	for _, sid := range []SessionID{1, 2, 4} {
		profile := entity.SyncProfile{Key: "near"}
		if sid == 2 {
			profile = entity.SyncProfile{Key: "far", LOD: 2}
		}
		mustSubscribe(t, manager, sid, 6102, profile)
	}
	mustFlush(t, manager)
	for _, sid := range []SessionID{1, 2, 4} {
		transport.take(sid)
	}
	mustSubscribe(t, manager, 3, 6102, entity.SyncProfile{Key: "near"})
	state.MarkDirty(1)
	mustFlush(t, manager)
	for _, sid := range []SessionID{1, 2, 3, 4} {
		got := oneFrame(t, transport, sid).updates[6102]
		wantProfile := "near"
		if sid == 2 {
			wantProfile = "far"
		}
		if got.Full != (sid == 3) || got.Profile.Key != wantProfile || got.Version != 1 {
			t.Fatalf("session %d: %+v", sid, got)
		}
	}
}

// 传输故意保留原始切片，验证之后的会话和 Flush 不会覆盖已准入字节。
func TestEncodeWorkspaceDoesNotMutateRetainedFrames(t *testing.T) {
	var retained, copies [][]byte
	transport := TransportFunc(func(_ context.Context, _ SessionID, data []byte) error {
		retained = append(retained, data)
		copies = append(copies, bytes.Clone(data))
		return nil
	})
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxFrameBytes: 180}})
	packs := 0
	states := make([]*entity.SubjectSyncState, 3)
	for i := range states {
		states[i] = testSubject(t, int64(i+1), &packs)
		if err := manager.Register(states[i]); err != nil {
			t.Fatal(err)
		}
	}
	open(t, manager, 1, 2)
	for _, sid := range []SessionID{1, 2} {
		for _, state := range states {
			mustSubscribe(t, manager, sid, state.SubjectID(), entity.SyncProfile{})
		}
	}
	mustFlush(t, manager)
	for range 8 {
		for _, state := range states {
			state.MarkDirty(4)
		}
		mustFlush(t, manager)
	}
	if len(retained) <= 18 {
		t.Fatalf("expected split frames, got %d", len(retained))
	}
	for i, data := range retained {
		if !bytes.Equal(data, copies[i]) {
			t.Fatalf("frame %d overwritten", i)
		}
		if _, err := frame.Decode(data, frame.DefaultLimits()); err != nil {
			t.Fatal(err)
		}
	}
	// 工作区清空活引用；容量可保留，组件载荷不能保留。
	for _, obj := range manager.encodeWork.objects[:cap(manager.encodeWork.objects)] {
		if obj.Components != nil {
			t.Fatal("object references retained")
		}
	}
	for _, component := range manager.encodeWork.components[:cap(manager.encodeWork.components)] {
		if component.Data != nil {
			t.Fatal("payload reference retained")
		}
	}
}

// 层级并集的优先级取各层级里最优的那个：与 SyncViewSet.Lookup 给并集的 Priority 同一条规则，校验对得上。
func TestUnionProfilePriorityIsItsBestLayer(t *testing.T) {
	near, far, team := entity.SyncProfile{Key: "near"}, entity.SyncProfile{Key: "far", LOD: 1}, entity.SyncProfile{Key: "team", LOD: 8}
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{ProfilePriorities: map[entity.SyncProfile]int{team: -5}})
	union := entity.UnionSyncProfiles(far, team)
	if m.CompareProfiles(union, near) >= 0 || m.CompareProfiles(entity.UnionSyncProfiles(far, near), far) >= 0 {
		t.Fatal("a union does not rank as its best layer")
	}
	views, err := entity.NewSyncViewSet(entity.SyncView{Profile: far, Priority: 1}, entity.SyncView{Profile: team, Priority: -5})
	if err != nil {
		t.Fatal(err)
	}
	view, _ := views.Lookup(union)
	if err := m.ValidateViewPriority(view); err != nil {
		t.Fatal(err)
	}
}
