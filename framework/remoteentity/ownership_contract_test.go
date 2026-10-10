package remoteentity

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"sync/atomic"
	"testing"
	"time"
)

type lostReplyModeStore struct {
	*lostReplyMarkerStore
	loseEnter, loseLeave bool
	failBefore           bool
}

func (s *lostReplyModeStore) EnterSharedExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if s.failBefore {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout before send")
	}
	next, err := s.mockMarkerStore.EnterSharedExpected(ctx, id, expected)
	if err != nil {
		return next, err
	}
	if s.afterTransfer != nil {
		s.afterTransfer()
	}
	if s.loseEnter {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout: enter-shared reply lost")
	}
	return next, nil
}

func (s *lostReplyModeStore) LeaveSharedExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if s.failBefore {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout before send")
	}
	next, err := s.mockMarkerStore.LeaveSharedExpected(ctx, id, expected)
	if err != nil {
		return next, err
	}
	if s.loseLeave {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout: leave-shared reply lost")
	}
	return next, nil
}

func modeSetup(t *testing.T, kind entity.EntityKind, seq int64) (*Manager, *lostReplyModeStore, *testRemoteEntity) {
	t.Helper()
	mgr, inner, live := indeterminateTransferSetup(t, kind, seq)
	store := &lostReplyModeStore{lostReplyMarkerStore: inner}
	mgr.SetOwnershipStore(store)
	return mgr, store, live
}

func probeAdmission(t *testing.T, mgr *Manager, live *testRemoteEntity) error {
	t.Helper()
	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err == nil {
		_ = batch.Abort(context.Background(), errors.New("probe"))
		_ = batch.Close(context.Background())
	}
	return err
}

func TestEnterSharedPromiseLostReplyFollowsAuthorityIntoSharedMode(t *testing.T) {
	mgr, store, live := modeSetup(t, 251, 1501)
	store.loseEnter = true
	ctx := context.Background()

	next, err := mgr.EnterRemoteSharedMode(ctx, live.GUId())
	authority := store.leases[live.GUId()]
	if !store.marks[live.GUId()] {
		t.Fatal("test premise: authority must be shared after the executed CAS")
	}
	state := live.RemoteOwnershipState()
	if state == entity.RemoteOwnershipLocalOwned {
		t.Fatalf("authority_shared=%v state=%s admission=%v: entered shared at authority but old local-owned write admitted",
			store.marks[live.GUId()], state, probeAdmission(t, mgr, live))
	}
	// 权威已确认进入共享:结果应与一次正常成功的 EnterShared 一致。
	if err != nil || !next.Shared || state != entity.RemoteOwnershipShared {
		t.Fatalf("enter-shared applied but reported err=%v next=%+v state=%s authority=%+v", err, next, state, authority)
	}
	if live.RemoteVersionVector().MarkerEpoch != authority.MarkerEpoch {
		t.Fatalf("live marker epoch %d != authority %d", live.RemoteVersionVector().MarkerEpoch, authority.MarkerEpoch)
	}
	// 之后的准入走共享路径(需要分布式锁),而不是独占快捷路径。
	if err := probeAdmission(t, mgr, live); err != nil {
		t.Fatalf("shared-mode admission after recovery: %v", err)
	}
	if w, ok := mgr.get(live.GUId()); !ok || !w.isMarked() {
		t.Fatal("wrapper marker did not follow the authority into shared mode")
	}
}

func TestEnterSharedPromiseLostReplyFreezesUntilAuthorityAnswers(t *testing.T) {
	mgr, store, live := modeSetup(t, 252, 1502)
	store.loseEnter = true
	store.afterTransfer = func() { store.blackout.Store(true) }
	ctx := context.Background()

	if _, err := mgr.EnterRemoteSharedMode(ctx, live.GUId()); err == nil {
		t.Fatal("unknown outcome with unreachable authority must not report success")
	}
	if state := live.RemoteOwnershipState(); state == entity.RemoteOwnershipLocalOwned || state == entity.RemoteOwnershipShared {
		t.Fatalf("unknown outcome restored a writable mode: live=%s", state)
	}
	if err := probeAdmission(t, mgr, live); err == nil {
		t.Fatal("write admitted while mode is unknown and authority unreachable")
	}
	store.blackout.Store(false)
	if err := probeAdmission(t, mgr, live); err != nil {
		t.Fatalf("after the authority answered (shared, ours): %v", err)
	}
	if state := live.RemoteOwnershipState(); state != entity.RemoteOwnershipShared {
		t.Fatalf("thawed into %s, want shared as the authority says", state)
	}
}

func TestLeaveSharedPromiseLostReplyFollowsAuthorityIntoLocalOwned(t *testing.T) {
	mgr, store, live := modeSetup(t, 253, 1503)
	ctx := context.Background()
	if _, err := mgr.EnterRemoteSharedMode(ctx, live.GUId()); err != nil {
		t.Fatal(err)
	}
	store.loseLeave = true
	next, err := mgr.LeaveRemoteSharedMode(ctx, live.GUId())
	if store.marks[live.GUId()] {
		t.Fatal("test premise: authority must have left shared mode")
	}
	if err != nil || next.Shared || live.RemoteOwnershipState() != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("leave-shared applied but reported err=%v next=%+v state=%s", err, next, live.RemoteOwnershipState())
	}
	// RR-20260913-13:旧实现把 live 恢复成 shared,后续普通写第一次因刷新后非 shared 被拒,
	// 第二、三次卡在 `shared -> local_owned` 非法迁移上永远不恢复。连续三次准入都必须成功。
	for attempt := 1; attempt <= 3; attempt++ {
		if err := probeAdmission(t, mgr, live); err != nil {
			t.Fatalf("attempt %d: repeated admission did not recover after authoritative refresh: %v (live=%s)", attempt, err, live.RemoteOwnershipState())
		}
	}
}

// 对照:发送前失败、Lua 没执行——恢复原模式,继续可写。
func TestSharedModePromiseFailureBeforeSendRestoresPreviousMode(t *testing.T) {
	mgr, store, live := modeSetup(t, 254, 1504)
	ctx := context.Background()
	store.failBefore = true
	if _, err := mgr.EnterRemoteSharedMode(ctx, live.GUId()); err == nil {
		t.Fatal("must report the send failure")
	}
	if live.RemoteOwnershipState() != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("proven no-op must restore local_owned, got %s", live.RemoteOwnershipState())
	}
	if err := probeAdmission(t, mgr, live); err != nil {
		t.Fatalf("before-send recovery failed: %v", err)
	}
	store.failBefore = false
	if _, err := mgr.EnterRemoteSharedMode(ctx, live.GUId()); err != nil {
		t.Fatal(err)
	}
	store.failBefore = true
	if _, err := mgr.LeaveRemoteSharedMode(ctx, live.GUId()); err == nil {
		t.Fatal("must report the send failure")
	}
	if live.RemoteOwnershipState() != entity.RemoteOwnershipShared {
		t.Fatalf("proven no-op must restore shared, got %s", live.RemoteOwnershipState())
	}
	if err := probeAdmission(t, mgr, live); err != nil {
		t.Fatalf("before-send recovery failed: %v", err)
	}
}

type lostReplyMarkerStore struct {
	*mockMarkerStore
	loseReply     atomic.Bool // 执行成功后返回 I/O 错误
	failBefore    atomic.Bool // 执行前就失败（对照：确定没执行）
	blackout      atomic.Bool // 之后的权威查询也不可用
	afterTransfer func()      // 执行成功后、返回前的钩子
}

func (s *lostReplyMarkerStore) TransferExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease, owner int32) (entity.RemoteEntityMarkerLease, error) {
	if s.failBefore.Load() {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout before send")
	}
	next, err := s.mockMarkerStore.TransferExpected(ctx, id, expected, owner)
	if err != nil {
		return next, err
	}
	if s.afterTransfer != nil {
		s.afterTransfer()
	}
	if s.loseReply.Load() {
		return entity.RemoteEntityMarkerLease{}, errors.New("i/o timeout: transfer reply lost")
	}
	return next, nil
}

func (s *lostReplyMarkerStore) GetOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	if s.blackout.Load() {
		return entity.RemoteEntityMarkerLease{}, false, errors.New("i/o timeout: marker store unavailable")
	}
	return s.mockMarkerStore.GetOwnership(ctx, id)
}

func indeterminateTransferSetup(t *testing.T, kind entity.EntityKind, seq int64) (*Manager, *lostReplyMarkerStore, *testRemoteEntity) {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := DefaultConfig()
	cfg.MarkerCacheTTL = time.Minute // 放大热 marker 的信任窗口，让旧行为稳定暴露
	cfg.OpTimeout = 2 * time.Second
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	store := &lostReplyMarkerStore{mockMarkerStore: newMockMarkerStore()}
	mgr.SetOwnershipStore(store)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	live := newTestRemoteEntity(seq, 1, kind)
	if err := live.SetRemoteVersionVector(entity.RemoteVersionVector{MarkerEpoch: 2, RouteEpoch: 3}); err != nil {
		t.Fatal(err)
	}
	loader.add(live)
	store.leases[live.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 2, RouteEpoch: 3}
	return mgr, store, live
}

func TestTransferPromiseLostReplyFencesOldOwnerWhenAuthorityConfirmsTransfer(t *testing.T) {
	mgr, store, live := indeterminateTransferSetup(t, 246, 1406)
	store.loseReply.Store(true)

	next, err := mgr.TransferRemoteOwnership(context.Background(), live.GUId(), 2000)
	authority := store.leases[live.GUId()]
	if authority.OwnerSid != 2000 {
		t.Fatalf("test premise: authority owner=%d, want 2000", authority.OwnerSid)
	}
	if batch, admitErr := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()}); !errors.Is(admitErr, entity.ErrRemoteFenced) {
		if admitErr == nil {
			_ = batch.Abort(context.Background(), errors.New("probe"))
			_ = batch.Close(context.Background())
		}
		t.Fatalf("lost reply: Redis owner=%d live=%s old owner write admitted=%v err=%v",
			authority.OwnerSid, live.RemoteOwnershipState(), admitErr == nil, admitErr)
	}
	if state := live.RemoteOwnershipState(); state != entity.RemoteOwnershipFenced {
		t.Fatalf("lost reply: Redis owner=%d live=%s, want fenced", authority.OwnerSid, state)
	}
	// 权威已经确认转移成功：结果应当和一次正常成功的 Transfer 一样。
	if err != nil || next.OwnerSid != 2000 {
		t.Fatalf("transfer applied but reported err=%v next=%+v", err, next)
	}
}

func TestTransferPromiseLostReplyFreezesOldOwnerUntilAuthorityAnswers(t *testing.T) {
	mgr, store, live := indeterminateTransferSetup(t, 247, 1407)
	store.loseReply.Store(true)
	// 回复丢失后权威也查不到——这才是真正的“未知”。
	store.afterTransfer = func() { store.blackout.Store(true) }

	_, err := mgr.TransferRemoteOwnership(context.Background(), live.GUId(), 2000)
	if err == nil {
		t.Fatal("transfer with unknown outcome and unreachable authority must not report success")
	}
	if state := live.RemoteOwnershipState(); state == entity.RemoteOwnershipLocalOwned || state == entity.RemoteOwnershipShared {
		t.Fatalf("unknown outcome restored old owner: live=%s", state)
	}
	// 权威不可用期间不能放行。
	if _, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()}); err == nil {
		t.Fatal("old owner write admitted while ownership is unknown and authority unreachable")
	}
	// 权威恢复后给出真相：已经转移，旧 owner 被 fence。
	store.blackout.Store(false)
	_, err = mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("after authority recovered: err=%v, want ErrRemoteFenced", err)
	}
}

// 对照：发送前就失败、Lua 没执行——权威仍是旧 owner，恢复本地所有权是正确的。
func TestTransferPromiseFailureBeforeSendRestoresOldOwner(t *testing.T) {
	mgr, store, live := indeterminateTransferSetup(t, 248, 1408)
	store.failBefore.Store(true)

	if _, err := mgr.TransferRemoteOwnership(context.Background(), live.GUId(), 2000); err == nil {
		t.Fatal("transfer must report the send failure")
	}
	if store.leases[live.GUId()].OwnerSid != 1000 {
		t.Fatal("test premise: authority must still name the old owner")
	}
	if state := live.RemoteOwnershipState(); state != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("proven no-op must restore local ownership, got %s", state)
	}
	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatalf("old owner must keep writing after a proven no-op: %v", err)
	}
	_ = batch.Abort(context.Background(), errors.New("probe"))
	_ = batch.Close(context.Background())
}
