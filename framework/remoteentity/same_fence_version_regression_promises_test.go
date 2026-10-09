package remoteentity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260927-15（OPEN-ITEMS C32，triage RR-11 项的残留加固）：同一 fence 下 StateVersion 不能回退。
//
// 非 authority 的兼容装配（ownership store 不实现 WriteAuthority，没有 GrantWrite）里，本节点独占写的每一笔都沿用实体当前的 LockFence，
// fence 不随写入递增。已提交事务 tx1 的迟到重放（投影器 / outbox）在 acknowledgeRemoteCommit 里先判断 remoteReceiptObsolete：
// 活实体版本恰等于 tx1.NextVersion 时不算过期，随后才 SetRemoteVersionVector。两步之间同实体的下一笔 tx2 已提交并把版本推进到 2
// ——fence 相同，SetRemoteVersionVector 只拒绝更小的 fence，于是 tx1 的旧向量把版本写回 1。之后下一写者以 BaseVersion=1 准备，
// 与权威（2）冲突被拒。正式装配每次写都经 GrantWrite 拿更大的 fence，旧向量被 ErrRemoteFenced 拒绝后重新判为过期，不受影响。
//
// 承诺：同 fence 下更小的 StateVersion 被拒绝，acknowledgeRemoteCommit 再次核对后把迟到重放判为过期（成功返回、不改向量）。
func TestSameFenceReplayCannotRewindStateVersion(t *testing.T) {
	const kind entity.EntityKind = 119
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	loader := newRemoteTestLoader()
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	live := &vectorPausingEntity{testRemoteEntity: newTestRemoteEntity(1486, 1, kind), reached: make(chan struct{}), resume: make(chan struct{})}
	loader.add(live)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	write := func(tx entity.RemoteTransactionID) []entity.RemoteCommit {
		t.Helper()
		batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
		if err != nil {
			t.Fatalf("prepare %s: %v", tx, err)
		}
		live.dirty.set(true)
		if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "same-fence", "", true, 0)); err != nil {
			t.Fatal(err)
		}
		commits := batch.Commits()
		if _, err = batch.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", tx, err)
		}
		if err = batch.Close(ctx); err != nil {
			t.Fatal(err)
		}
		return commits
	}
	tx1, tx2, tx3 := remoteTestTxID(0xF1), remoteTestTxID(0xF2), remoteTestTxID(0xF3)
	commits1 := write(tx1)
	if got := live.RemoteVersionVector(); got.StateVersion != 1 || got.LockFence != commits1[0].LockFence {
		t.Fatalf("premise: vector after tx1=%+v", got)
	}

	// tx1 的迟到重放通过过期判断（版本恰等于 NextVersion），停在写向量之前。
	live.armed.Store(true)
	replay := make(chan error, 1)
	go func() {
		_, err := mgr.ApplyRemoteCommits(context.Background(), tx1, commits1)
		replay <- err
	}()
	awaitSignal(t, ctx, live.reached, "the replay to reach SetRemoteVersionVector")

	// 同实体下一笔在同一 fence 下提交，版本推进到 2。
	commits2 := write(tx2)
	if commits2[0].LockFence != commits1[0].LockFence {
		t.Fatalf("premise: non-authority assembly must reuse the fence (tx1 %d, tx2 %d)", commits1[0].LockFence, commits2[0].LockFence)
	}
	advanced := live.RemoteVersionVector()
	if advanced.StateVersion != 2 {
		t.Fatalf("premise: vector after tx2=%+v, want version 2", advanced)
	}

	close(live.resume)
	if err := <-replay; err != nil {
		t.Fatalf("late replay of committed tx1: %v", err)
	}
	if got := live.RemoteVersionVector(); got != advanced {
		t.Fatalf("late replay of tx1 rewound the version vector under the same fence: %+v -> %+v", advanced, got)
	}
	// 下一写者按未回退的版本准备并提交。
	commits3 := write(tx3)
	if commits3[0].BaseVersion != 2 || live.RemoteVersionVector().StateVersion != 3 {
		t.Fatalf("next writer base=%d vector=%+v, want base 2 and version 3", commits3[0].BaseVersion, live.RemoteVersionVector())
	}
}

// vectorPausingEntity 在 armed 之后第一次 SetRemoteVersionVector 处停住（写入之前），让重放与下一写者按测试顺序交错。
type vectorPausingEntity struct {
	*testRemoteEntity
	armed   atomic.Bool
	reached chan struct{}
	resume  chan struct{}
}

func (e *vectorPausingEntity) SetRemoteVersionVector(version entity.RemoteVersionVector) error {
	if e.armed.CompareAndSwap(true, false) {
		close(e.reached)
		<-e.resume
	}
	return e.testRemoteEntity.SetRemoteVersionVector(version)
}
