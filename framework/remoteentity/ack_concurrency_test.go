package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// flakyPublicationStorage 让下一次 CommitRemote（投影器发布路径上的重复提交）返回瞬时错误，模拟“投影器结果未知”。
type flakyPublicationStorage struct {
	*MongoCommitter
	failNextCommit atomic.Bool
}

func (s *flakyPublicationStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if s.failNextCommit.CompareAndSwap(true, false) {
		return entity.RemoteCommitReceipt{}, errors.New("injected transient publication failure")
	}
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

// RR-20260926-63（W-2026-09-26-01 根因，REPRO-2026-09-26-05 §10 改写）：投影器报告“结果未知”（tracker Indeterminate）后，
// finalizer 按 RR-38 回源并自行发布；投影器对同一事务的重试同时走 afterRemoteCommit → AcknowledgeRemoteCommit。
// 两条路径对同一提交并发确认是设计允许的，所以 IRemoteCommitParticipant.AcknowledgeRemoteCommit 必须幂等且并发安全
// （生成实体经 dataengine.Tracker.AdvanceVersion 原子 CAS）。以 -race 运行：参与者实现（含测试替身）不得有数据竞争，
// 且两条路径都结束后写额度归还、实体确认到本提交的版本。
func TestProjectorRetryAndFinalizerAcknowledgeConcurrently(t *testing.T) {
	const kind entity.EntityKind = 125
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	for i := 0; i < 200; i++ {
		store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
		storage := &flakyPublicationStorage{MongoCommitter: store}
		loader := newRemoteTestLoader()
		live := newTestRemoteEntity(int64(3000+i), 1, kind)
		loader.add(live)
		backend, err := NewBackend(loader, storage)
		if err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.FinalizeRetryInterval = time.Millisecond
		mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
		mgr.SetBackend(backend)
		mgr.SetOwnershipStore(store)
		mgr.SetSyncer(&countingSnapshotSyncer{})
		batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
		if err != nil {
			t.Fatal(err)
		}
		live.dirty.set(true)
		tx := remoteTestTxID(byte(i%250 + 1))
		if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "async", "", true, 1)); err != nil {
			t.Fatal(err)
		}
		commits := batch.Commits()
		if _, err = batch.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err = batch.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		// 投影器第 1 次：Mongo 事务已提交，发布报告未知 → tracker Indeterminate，finalizer 随即回源并发布。
		if _, err = store.CommitRemoteBatch(context.Background(), commits); err != nil {
			t.Fatal(err)
		}
		storage.failNextCommit.Store(true)
		_, _ = mgr.ApplyRemoteCommits(context.Background(), tx, commits)
		// 投影器第 2 次与 finalizer 的回源发布并发确认同一提交。
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = mgr.ApplyRemoteCommits(context.Background(), tx, commits)
		}()
		deadline := time.Now().Add(3 * time.Second)
		for len(mgr.remote.writeSlots) != 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Microsecond)
		}
		wg.Wait()
		if slots := len(mgr.remote.writeSlots); slots != 0 {
			t.Fatalf("iteration %d: write slots=%d after both acknowledgement paths finished", i, slots)
		}
		if got, want := live.RemoteVersionVector().StateVersion, commits[0].NextVersion; got != want {
			t.Fatalf("iteration %d: entity acknowledged at version %d, want %d", i, got, want)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = mgr.StopFinalizer(ctx)
		cancel()
	}
}
