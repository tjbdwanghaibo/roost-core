package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

const rr44Kind entity.EntityKind = 213

func newRR44Manager(t *testing.T) (*Manager, *remoteTestLoader) {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: rr44Kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	m := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	loader := newRemoteTestLoader()
	m.SetBackend(loader)
	m.SetOwnershipStore(newMockMarkerStore())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = m.StopFinalizer(ctx)
	})
	return m, loader
}

// countingLocalExecutor 模拟慢阶段注入的快池执行器：每次调用就是一次快池续行。
func countingLocalExecutor(hops *atomic.Int32) context.Context {
	return entity.WithLocalExecutor(context.Background(), func(fn func()) error { hops.Add(1); fn(); return nil })
}

// RR-20260926-44：Prepare 在慢阶段失败时没有任何已定稿的本地状态，Abort 不得再投递快池续行；
// 已经取得的前序实体仍须由 Close 释放。
func TestPrepareRemoteWriteBatchFailureAbortsWithoutFastPoolHop(t *testing.T) {
	m, loader := newRR44Manager(t)
	present := newTestRemoteEntity(9301, 1, rr44Kind)
	loader.add(present)
	missing := testRemoteFullIDWithKind(9302, 1, rr44Kind)

	var hops atomic.Int32
	ctx := countingLocalExecutor(&hops)
	if _, err := m.PrepareRemoteWriteBatch(ctx, []int64{missing}); err == nil {
		t.Fatal("prepare of a missing entity must fail")
	}
	if n := hops.Load(); n != 0 {
		t.Fatalf("single-entity Prepare failure made %d fast-pool continuation(s); nothing was finalized", n)
	}

	// 多实体：前一个实体已取得写权限，后一个失败。
	if _, err := m.PrepareRemoteWriteBatch(ctx, []int64{present.GUId(), missing}); err == nil {
		t.Fatal("prepare with a missing entity must fail")
	}
	if n := hops.Load(); n != 0 {
		t.Fatalf("multi-entity Prepare failure made %d fast-pool continuation(s); nothing was finalized", n)
	}
	// 失败路径的唯一收尾（Close）必须释放前序实体的写门与写许可。
	next, err := m.PrepareRemoteWriteBatch(context.Background(), []int64{present.GUId()})
	if err != nil {
		t.Fatalf("entity acquired before the failure was not released: %v", err)
	}
	_ = next.Abort(context.Background(), errors.New("test cleanup"))
	_ = next.Close(context.Background())
	if stats := m.Stats(); stats.WritesInFlight != 0 {
		t.Fatalf("write slots leaked: %+v", stats)
	}
}

// RR-20260926-44：未定稿的批次（handler 在定稿前失败）Abort 就地置 aborted，不走快池；
// 之后不能再定稿，Close 仍释放。
func TestRemoteWriteBatchAbortBeforeFinalizeStaysInPlace(t *testing.T) {
	m, loader := newRR44Manager(t)
	live := newTestRemoteEntity(9303, 1, rr44Kind)
	loader.add(live)
	batch, err := m.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	var hops atomic.Int32
	if err := batch.Abort(countingLocalExecutor(&hops), errors.New("handler failed")); err != nil {
		t.Fatal(err)
	}
	if n := hops.Load(); n != 0 {
		t.Fatalf("Abort of an unfinalized batch made %d fast-pool continuation(s)", n)
	}
	live.dirty.set(true)
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(44), "probe", "", true, 0)); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("aborted batch must refuse FinalizeLocked, got %v", err)
	}
	if err := batch.Abort(countingLocalExecutor(&hops), errors.New("again")); err != nil || hops.Load() != 0 {
		t.Fatalf("repeated Abort err=%v hops=%d", err, hops.Load())
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := m.Stats(); stats.WritesInFlight != 0 {
		t.Fatalf("write slots leaked: %+v", stats)
	}
}

// 负对照：已定稿的批次有本地状态要回滚，Abort 仍经本地执行器（快池）执行回滚。
func TestRemoteWriteBatchAbortAfterFinalizeRollsBackOnLocalExecutor(t *testing.T) {
	m, loader := newRR44Manager(t)
	live := newTestRemoteEntity(9304, 1, rr44Kind)
	loader.add(live)
	batch, err := m.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.set(true)
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(45), "probe", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	if len(batch.Commits()) != 1 || live.dirty.Dirty() {
		t.Fatalf("setup: finalize must freeze one commit, commits=%d dirty=%v", len(batch.Commits()), live.dirty.Dirty())
	}
	var hops atomic.Int32
	var ranOnExecutor atomic.Bool
	ctx := entity.WithLocalExecutor(context.Background(), func(fn func()) error {
		hops.Add(1)
		fn()
		ranOnExecutor.Store(live.dirty.Dirty())
		return nil
	})
	if err := batch.Abort(ctx, errors.New("rejected")); err != nil {
		t.Fatal(err)
	}
	if hops.Load() != 1 || !ranOnExecutor.Load() {
		t.Fatalf("finalized rollback must run once on the local executor: hops=%d rolledBackThere=%v", hops.Load(), ranOnExecutor.Load())
	}
	if len(batch.Commits()) != 0 {
		t.Fatalf("aborted batch still exposes commits: %d", len(batch.Commits()))
	}
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("aborted batch must not commit, got %v", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
