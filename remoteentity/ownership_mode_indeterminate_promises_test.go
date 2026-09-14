package remoteentity

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0189 · C8 · RR-20260913-12:EnterShared / LeaveShared 的 CAS 结果未知时,不能把 I/O 错误
// 当成"没执行"而恢复旧模式。U-0188 只改了 Transfer;Enter/Leave 仍对任何存储错误
// restoreOwnershipAfterTransitionFailure(expected):EnterSharedExpected 的 Lua 已把权威改成
// shared、回复丢失,本地回到 local_owned、热 marker 不清,随后 PrepareRemoteWriteBatch 走独占
// 快捷准入——与权威的共享模式不一致,跳过共享写所需的分布式锁协调。承诺:复用 Transfer 的
// "失效 marker → 独立有界重查 → 按权威结果收尾/冻结"骨架,按共享模式目标判定。

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
