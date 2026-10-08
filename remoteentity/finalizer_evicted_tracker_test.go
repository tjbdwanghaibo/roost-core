package remoteentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// OPEN-ITEMS B13：RR-20260926-37 / 38 复核补充——Durability 1/2 的延迟收尾项，若其 tracker 已结束并被容量 / TTL 淘汰，
// finalizer 必须直接回源拿持久结论，而不是等满 FinalizeProjectionTimeout。
//
// tracker 只在结束（Committed / Rejected / Indeterminate）之后才可能被淘汰，淘汰说明结论已经有了；RR-38 首版对 done==nil
// 仍进入“等投影器结论”分支，等到期限（默认 30s）才回源，期间 gate 与写额度一直被占。
// 这里用真实容量淘汰（TransactionTrackLimit=1 + 下一笔准入）在收尾入队前淘汰目标 tracker，期限保持 30s，
// 断言 finalizer 远早于期限就调了 CommitStatus，并释放 gate 让下一写者进入。
func TestFinalizerQueriesDurableStatusWhenTrackerWasEvicted(t *testing.T) {
	const kind entity.EntityKind = 119
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	loader := &statusSignalLoader{remoteTestLoader: newRemoteTestLoader(), statusCalls: make(chan struct{}, 1)}
	cfg := DefaultConfig()
	cfg.TransactionTrackLimit = 1
	cfg.FinalizeProjectionTimeout = 30 * time.Second
	cfg.FinalizeRetryInterval = 10 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(stop)
	})
	live := newTestRemoteEntity(1497, 1, kind)
	loader.add(live)
	ctx := context.Background()

	batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.set(true)
	tx := remoteTestTxID(0xE1)
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "evicted", "", true, 1)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	if _, err = batch.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 投影器报告持久结论（与 WAL 投影同一入口），tracker 结束。
	if _, err = mgr.ApplyRemoteCommits(ctx, tx, commits); err != nil {
		t.Fatal(err)
	}
	if err := mgr.FlushRemoteTransaction(ctx, tx); err != nil {
		t.Fatal(err)
	}
	// 下一笔准入按容量淘汰最旧的已结束记录——目标 tracker。
	other := remoteTestTxID(0xE2)
	if err = mgr.trackRemoteTransaction(other); err != nil {
		t.Fatal(err)
	}
	mgr.completeRemoteTransaction(other, entity.RemoteCommitStatus{TransactionID: other, State: entity.RemoteCommitCommitted})
	if _, done := mgr.trackedRemoteOutcome(tx); done != nil {
		t.Fatal("premise: the finished tracker was not evicted by capacity")
	}
	select {
	case <-loader.statusCalls:
	default:
	}

	started := time.Now()
	if err = batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-loader.statusCalls:
	case <-wait.Done():
		t.Fatalf("finalizer did not query the durable status within %v of Close (FinalizeProjectionTimeout=%v): it waited on an evicted tracker",
			time.Since(started), cfg.FinalizeProjectionTimeout)
	}
	next, err := mgr.PrepareRemoteWriteBatch(wait, []int64{live.GUId()})
	if err != nil {
		t.Fatalf("gate still held after the finalizer read the durable status: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= cfg.FinalizeProjectionTimeout/2 {
		t.Fatalf("finalizer released the gate after %v, want well before FinalizeProjectionTimeout", elapsed)
	}
	_ = next.Abort(ctx, errors.New("test cleanup"))
	_ = next.Close(ctx)
	stop, stopCancel := context.WithTimeout(ctx, 3*time.Second)
	defer stopCancel()
	if err = mgr.StopFinalizer(stop); err != nil {
		t.Fatal(err)
	}
	if slots := len(mgr.remote.writeSlots); slots != 0 {
		t.Fatalf("write slots leaked: %d", slots)
	}
}

// statusSignalLoader 在每次回源读持久状态时发信号。
type statusSignalLoader struct {
	*remoteTestLoader
	statusCalls chan struct{}
}

func (l *statusSignalLoader) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	select {
	case l.statusCalls <- struct{}{}:
	default:
	}
	return l.remoteTestLoader.CommitStatus(ctx, id)
}
