package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0188 · C8 · RR-20260913-09：Transfer 的外部 CAS 结果未知时，不能把 I/O 错误当成
// “没执行”而恢复旧 owner 的写权限。旧实现对 TransferExpected 的任何错误都调用
// restoreOwnershipAfterTransitionFailure：live 回到 local_owned，热 marker 仍是旧 lease，
// MarkerCacheTTL 内 PrepareRemoteWriteBatch 直接放行——而权威里 owner 已经是新节点。
// 承诺：回复丢失后失效本地 marker，用独立的有界 context 重查权威；权威说已转移就按
// 转移成功收尾（fenced），权威查不到就冻结（recovering、不可写），只有权威确认没变才恢复。

// lostReplyMarkerStore 让 Transfer 的 Lua 真正执行（owner 改成新节点）后再丢掉回复。
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
