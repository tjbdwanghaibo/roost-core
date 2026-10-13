// Finalizer：持久结论的回源与收尾、停机后拒绝新的移交、投影器负责发布时不重复发布、sid 作用域数据在 WAL 准入前拒绝。

package remoteentity

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

func (e *reloadableFullDocEntity) RollbackRemoteCommit(entity.RemoteCommit) {
	e.rollbacks.Add(1)
	if !fctx.InShortWorker() {
		e.rollbacksOffFast.Add(1)
	}
}

func TestFinalizerRejectionRollbackRunsOnNestFastWorker(t *testing.T) {
	for i, tc := range []struct {
		name       string
		durability uint8
	}{
		{"memory_unreachable", 0},
		{"strict_rejected", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, live := newReloadFixture(t, 1961+int64(i))
			engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithRemoteEntityManager(entity.IRemoteEntityManager(f.mgr)), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
			if f.mgr.localExecutor.Load() == nil {
				t.Fatal("premise: NewEngine did not bind NestMgr.RunLocal to the Remote Manager")
			}
			tx := remoteTestTxID(0xD0 + byte(i))
			f.storage.unreachable.Store(tc.durability == 0)
			batch := f.prepareRejected(t, live, tx, tc.durability)
			switch tc.durability {
			case 0:
				if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
					t.Fatalf("commit err=%v, want indeterminate (finalizer resolves it)", err)
				}
			case 2:
				f.mgr.RejectRemoteTransaction(tx, "lease expired")
				if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteRejected) {
					t.Fatalf("strict commit err=%v, want rejected (finalizer settles it)", err)
				}
			}
			if err := batch.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.assertReloadedFromAuthority(t, live) // finalizer 已收尾：回滚、隔离、释放 gate、卸载
			if n := live.rollbacks.Load(); n != 1 {
				t.Fatalf("RollbackRemoteCommit ran %d time(s), want once", n)
			}
			if off := live.rollbacksOffFast.Load(); off != 0 {
				t.Fatalf("finalizer rejection rollback ran off the Nest fast pool (%d call(s)); with Nest bound it must go through NestMgr.RunLocal", off)
			}
		})
	}
}

func TestDeferredCloseIsRefusedOnceTheFinalizerHasStopped(t *testing.T) {
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.StartFinalizer()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}

	// One attempt could pick the Done branch by chance; many cannot all be
	// chance. Every one of them has to be refused.
	const attempts = 64
	accepted := 0
	for i := range attempts {
		item := deferredRemoteClose{txID: remoteTestTxID(byte(i))}
		if err := manager.deferRemoteClose(item); err == nil {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("%d of %d deferred closes were handed to a queue nobody drains; their entries and finalize slots leak",
			accepted, attempts)
	}
	if queued := len(manager.remote.finalizeQueue); queued != 0 {
		t.Fatalf("%d items are sitting in the finalize queue after the stop completed", queued)
	}
}

// A handoff that starts before the stop must still be drained: refusing it
// would be just as wrong as accepting one too late, because the caller has
// already given up ownership of the entries.
func TestDeferredCloseAcceptedBeforeTheStopIsStillDrained(t *testing.T) {
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.StartFinalizer()

	if err := manager.deferRemoteClose(deferredRemoteClose{txID: remoteTestTxID(7)}); err != nil {
		t.Fatalf("a handoff before the stop must be accepted: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
	if queued := len(manager.remote.finalizeQueue); queued != 0 {
		t.Fatalf("%d items were left in the queue after the stop drained it", queued)
	}
}

// Durability 1/2 的延迟收尾项，若其 tracker 已结束并被容量 / TTL 淘汰，finalizer 直接回源拿持久结论，而不是等满
// FinalizeProjectionTimeout（淘汰说明结论已经有了）。用真实容量淘汰（TransactionTrackLimit=1 + 下一笔准入）在收尾入队前淘汰
// 目标 tracker，期限保持 30s，断言 finalizer 远早于期限就调了 CommitStatus，并释放 gate 让下一写者进入。
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

// B14：原两个发布者在解冻中途相遇的路径已收敛。唯一 outbox 发布者停在
// Recovering 时，投影器重放只报告持久结论，不抢着解冻或重新发布；放行后资源完整收尾。
func TestOutboxMidThawDoesNotAdmitASecondPublisher(t *testing.T) {
	f, live := newMidThawFixture(t, 1894)
	tx := remoteTestTxID(194)
	commits := f.asyncWrite(t, tx)
	f.projectorReportsUnknown(t, tx, commits)
	failuresBefore := remoteApplyErrors()
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	live.holdLocalOwned.Store(true)
	release := sync.OnceFunc(func() { close(live.localOwnedResume) })
	t.Cleanup(release)
	f.answerPoll(t, true)
	awaitSignal(t, deadline(t), live.localOwnedReached, "the outbox's thaw to reach Recovering")
	if got := live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipRecovering {
		t.Fatalf("state=%v", got)
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatal(err)
	}
	if got := f.syncer.published.Load(); got != 0 {
		t.Fatalf("published during paused thaw: %d", got)
	}
	release()
	f.finishPublication(t)
	if got := remoteApplyErrors(); got != failuresBefore {
		t.Fatalf("new apply errors: %d -> %d", failuresBefore, got)
	}
	want := entity.RemoteVersionVector{StateVersion: commits[0].NextVersion, MarkerEpoch: commits[0].MarkerEpoch, LockFence: commits[0].LockFence, RouteEpoch: commits[0].RouteEpoch}
	if got := live.RemoteVersionVector(); got != want {
		t.Fatalf("version=%+v want=%+v", got, want)
	}
	if status, err := f.mgr.RemoteCommitStatus(context.Background(), tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	select {
	case step := <-f.steps:
		t.Fatalf("continued after release: %s", step)
	default:
	}
}

type midThawRemoteEntity struct {
	*pausingRemoteEntity
	holdLocalOwned    atomic.Bool
	localOwnedReached chan struct{}
	localOwnedResume  chan struct{}
}

func (e *midThawRemoteEntity) TransitionRemoteOwnership(to entity.RemoteOwnershipState) error {
	if to == entity.RemoteOwnershipLocalOwned && e.holdLocalOwned.CompareAndSwap(true, false) {
		close(e.localOwnedReached)
		<-e.localOwnedResume
	}
	return e.pausingRemoteEntity.TransitionRemoteOwnership(to)
}

// newMidThawFixture 与 newProjectionOwnershipFixture 相同，只是 loader 里放的是 midThawRemoteEntity。
func newMidThawFixture(t *testing.T, id int64) (projectionOwnershipFixture, *midThawRemoteEntity) {
	t.Helper()
	const kind entity.EntityKind = 124
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	storage := &steppedStatusStorage{MongoCommitter: store, polled: make(chan struct{}), answer: make(chan bool)}
	loader := newRemoteTestLoader()
	live := &midThawRemoteEntity{
		pausingRemoteEntity: &pausingRemoteEntity{testRemoteEntity: newTestRemoteEntity(id, 1, kind), reached: make(chan struct{}), resume: make(chan struct{})},
		localOwnedReached:   make(chan struct{}), localOwnedResume: make(chan struct{}),
	}
	loader.add(live)
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	storage.finalizerCtx = mgr.remote.finalizeCtx
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(store)
	syncer := &countingSnapshotSyncer{}
	mgr.SetSyncer(syncer)
	steps := make(chan string, 64)
	mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { steps <- step }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	return projectionOwnershipFixture{mgr: mgr, store: store, storage: storage, syncer: syncer, live: live.pausingRemoteEntity, steps: steps}, live
}

// remoteApplyErrors 是 publisher 入口（投影器与 outbox 重放共用的 ApplyRemoteCommits）的失败累计。
func remoteApplyErrors() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "remote_entity.remote.apply_total" && metric.Labels["result"] == "error" {
			return metric.Value
		}
	}
	return 0
}

func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// latencyCountingStorage 统计 finalizer 回源读取，并给每次后端调用加固定延迟以模拟网络往返；
// 对照两侧用同一份夹具，只比较 Remote 收尾链路本身的差异。
type latencyCountingStorage struct {
	*MongoCommitter
	latency     time.Duration
	statusReads atomic.Int64
}

func (s *latencyCountingStorage) pause() {
	if s.latency > 0 {
		time.Sleep(s.latency)
	}
}

func (s *latencyCountingStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	s.statusReads.Add(1)
	s.pause()
	return s.MongoCommitter.CommitStatus(ctx, id)
}

func (s *latencyCountingStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	s.pause()
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

func (s *latencyCountingStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	s.pause()
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

func loadEnvInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}

// TestRemoteAsyncFinalizerProjectionLoad 是 Remote 异步收尾链路的负载对照入口（默认跳过）：
// 真实 nestwal WAL + engine.Projector/MongoStore + mongotest，Remote 走正式 Backend + MongoCommitter。
// W 个写者各自在独立实体上连续做 Durability 1 写，统计投影完成吞吐、finalizer 回源读取次数与快照发布次数。
//
//	ROOST_REMOTE_FINALIZE_LOAD=1 ROOST_REMOTE_FINALIZE_WRITES=2000 ROOST_REMOTE_FINALIZE_WRITERS=16 \
//	ROOST_REMOTE_FINALIZE_LATENCY_US=1000 GOWORK=off go test -count=1 -run TestRemoteAsyncFinalizerProjectionLoad -v ./framework/remoteentity
func TestRemoteAsyncFinalizerProjectionLoad(t *testing.T) {
	if os.Getenv("ROOST_REMOTE_FINALIZE_LOAD") != "1" {
		t.Skip("set ROOST_REMOTE_FINALIZE_LOAD=1 to run the finalizer/projection comparison")
	}
	writes := loadEnvInt("ROOST_REMOTE_FINALIZE_WRITES", 2000)
	writers := loadEnvInt("ROOST_REMOTE_FINALIZE_WRITERS", 16)
	latency := time.Duration(loadEnvInt("ROOST_REMOTE_FINALIZE_LATENCY_US", 0)) * time.Microsecond
	// 每个写者轮换的实体数：1 = 同实体连续写（受 gate 持有时间限制）；更大时写入分散到独立实体，
	// 吞吐主要由投影器决定。
	perWriter := loadEnvInt("ROOST_REMOTE_FINALIZE_ENTITIES_PER_WRITER", 1)
	const kind entity.EntityKind = 126
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	mongo := newRemoteMongoFake()
	committer := NewMongoCommitter(mongo, "control", 1000, 0)
	storage := &latencyCountingStorage{MongoCommitter: committer, latency: latency}
	loader := newRemoteTestLoader()
	lives := make([][]*testRemoteEntity, writers)
	for i := range lives {
		for j := range perWriter {
			live := newTestRemoteEntity(int64(26000+i*perWriter+j), 1, kind)
			loader.add(live)
			lives[i] = append(lives[i], live)
		}
	}
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.MaxConcurrentWrites = max(4*writers, writers*perWriter)
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(committer)
	syncer := &countingSnapshotSyncer{}
	mgr.SetSyncer(syncer)
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(stop)
	})
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 1000, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(committer, mgr); err != nil {
		t.Fatal(err)
	}
	walOptions := nestwal.DefaultOptions(t.TempDir())
	walOptions.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(walOptions)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{
		CloseWAL: true, IdlePoll: 5 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = projector.Close(closing)
	})

	var seq atomic.Uint64
	var failures atomic.Int64
	started := time.Now()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(owned []*testRemoteEntity) {
			defer wg.Done()
			for i := range writes / writers {
				live := owned[i%len(owned)]
				batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
				if err != nil {
					failures.Add(1)
					t.Errorf("prepare: %v", err)
					return
				}
				n := seq.Add(1)
				var tx entity.RemoteTransactionID
				tx[0] = 0x38
				binary.BigEndian.PutUint64(tx[8:], n)
				live.dirty.set(true)
				if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "load", "", true, uint8(nest.DurabilityAsync.Record()))); err != nil {
					failures.Add(1)
					t.Errorf("finalize: %v", err)
					return
				}
				if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync.Record(), batch.Commits())); err != nil {
					failures.Add(1)
					t.Errorf("wal: %v", err)
					return
				}
				if _, err = batch.Commit(ctx); err != nil {
					failures.Add(1)
					t.Errorf("commit: %v", err)
					return
				}
				projector.TransactionReleased(coredata.TransactionID(tx))
				if err = batch.Close(ctx); err != nil {
					failures.Add(1)
					t.Errorf("close: %v", err)
					return
				}
			}
		}(lives[w])
	}
	wg.Wait()
	if err = projector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for len(mgr.remote.writeSlots) != 0 {
		if ctx.Err() != nil {
			t.Fatalf("write slots not released: %d", len(mgr.remote.writeSlots))
		}
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(started)
	total := int64(writes / writers * writers)
	stats := projector.Stats()
	fmt.Printf("RR38LOAD entities_per_writer=%d writes=%d writers=%d latency=%s elapsed=%s tps=%.1f status_reads=%d reads_per_write=%.3f snapshot_publishes=%d publishes_per_write=%.3f failures=%d projection_failures=%d wal_unacked=%d\n",
		perWriter, total, writers, latency, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), storage.statusReads.Load(),
		float64(storage.statusReads.Load())/float64(total), syncer.published.Load(), float64(syncer.published.Load())/float64(total),
		failures.Load(), stats.ProjectionFailures, stats.WALUnacked)
}

// Durability 1/2 的 Remote 事务由 WAL 投影器负责完成（Mongo 原子提交 + 发布 + ack）。finalizer 只在投影器报告未知（tracker
// Indeterminate）或超过较长期限后回源；两个发布者碰到同一事务时，acknowledgeRemoteCommit 必须幂等。所有交错都由通道控制，不依赖 sleep。
// 历史：RR-20260926-38。

// pausingRemoteEntity 在 armed 之后的第一次 RemoteOwnershipState 读取处停住，
// 让两个发布者按测试指定的顺序交错。
type pausingRemoteEntity struct {
	*testRemoteEntity
	armed   atomic.Bool
	reached chan struct{}
	resume  chan struct{}
}

func (e *pausingRemoteEntity) RemoteOwnershipState() entity.RemoteOwnershipState {
	state := e.testRemoteEntity.RemoteOwnershipState()
	if e.armed.CompareAndSwap(true, false) {
		close(e.reached)
		<-e.resume
	}
	return state
}

// steppedStatusStorage 让测试逐次应答 finalizer 的回源读取：每次 CommitStatus 先在 polled 上报到，
// 再等 answer（true 读 Mongo 真实状态，false 回答 Unknown）。failNextCommit 让投影器的下一次
// 发布调用在后端提交处返回瞬时错误（事务状态 Indeterminate，投影器“报告未知”）。
type steppedStatusStorage struct {
	*MongoCommitter
	finalizerCtx   context.Context
	statusCalls    atomic.Int32
	polled         chan struct{}
	answer         chan bool
	failNextCommit atomic.Bool
}

func (s *steppedStatusStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	// 这里只控制 finalizer 回源的交错；提交路径现在也会读取持久状态，它不属于此屏障。
	if ctx != s.finalizerCtx {
		return s.MongoCommitter.CommitStatus(ctx, id)
	}
	s.statusCalls.Add(1)
	select {
	case s.polled <- struct{}{}:
	case <-ctx.Done():
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, ctx.Err()
	}
	var open bool
	select {
	case open = <-s.answer:
	case <-ctx.Done():
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, ctx.Err()
	}
	if !open {
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, nil
	}
	return s.MongoCommitter.CommitStatus(ctx, id)
}

func (s *steppedStatusStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if s.failNextCommit.CompareAndSwap(true, false) {
		return entity.RemoteCommitReceipt{}, errors.New("injected transient publication failure")
	}
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

type countingSnapshotSyncer struct{ published atomic.Int32 }

func (s *countingSnapshotSyncer) PublishRemoteSnapshot(context.Context, entity.RemoteSnapshotRecord) error {
	s.published.Add(1)
	return nil
}
func (*countingSnapshotSyncer) DeleteRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, uint64) error {
	return nil
}
func (*countingSnapshotSyncer) PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error {
	return nil
}

type projectionOwnershipFixture struct {
	mgr     *Manager
	store   *MongoCommitter
	storage *steppedStatusStorage
	syncer  *countingSnapshotSyncer
	live    *pausingRemoteEntity
	steps   chan string
}

func newProjectionOwnershipFixture(t *testing.T, id int64) projectionOwnershipFixture {
	t.Helper()
	const kind entity.EntityKind = 124
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	storage := &steppedStatusStorage{MongoCommitter: store, polled: make(chan struct{}), answer: make(chan bool)}
	loader := newRemoteTestLoader()
	live := &pausingRemoteEntity{testRemoteEntity: newTestRemoteEntity(id, 1, kind), reached: make(chan struct{}), resume: make(chan struct{})}
	loader.add(live)
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	storage.finalizerCtx = mgr.remote.finalizeCtx
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(store)
	syncer := &countingSnapshotSyncer{}
	mgr.SetSyncer(syncer)
	steps := make(chan string, 64)
	mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { steps <- step }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// 让可能停在回源读取上的 finalizer 退出：StopFinalizer 取消 finalizeCtx。
		_ = mgr.StopFinalizer(ctx)
	})
	return projectionOwnershipFixture{mgr: mgr, store: store, storage: storage, syncer: syncer, live: live, steps: steps}
}

// asyncWrite：Durability 1 写入，Commit 返回推测回执，Close 把 gate/写额度交给 finalizer。
func (f projectionOwnershipFixture) asyncWrite(t *testing.T, tx entity.RemoteTransactionID) []entity.RemoteCommit {
	t.Helper()
	batch, err := f.mgr.PrepareRemoteWriteBatch(context.Background(), []int64{f.live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	f.live.dirty.set(true)
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
	return commits
}

func (f projectionOwnershipFixture) nextStep(t *testing.T, what string) string {
	t.Helper()
	select {
	case step := <-f.steps:
		return step
	case <-f.storage.polled:
		t.Fatalf("%s: finalizer read the durable status from the backend (reads=%d) while the WAL projector still owned the outcome",
			what, f.storage.statusCalls.Load())
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: finalizer made no progress", what)
	}
	return ""
}

func (f projectionOwnershipFixture) answerPoll(t *testing.T, open bool) {
	t.Helper()
	select {
	case <-f.storage.polled:
	case <-time.After(3 * time.Second):
		t.Fatal("finalizer did not read the durable status")
	}
	f.storage.answer <- open
}

func (f projectionOwnershipFixture) awaitStep(t *testing.T, want string) {
	t.Helper()
	select {
	case step := <-f.steps:
		if step != want {
			t.Fatalf("finalizer step=%s, want %s (owner=%v slots=%d)", step, want, f.live.testRemoteEntity.RemoteOwnershipState(), len(f.mgr.remote.writeSlots))
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("finalizer did not reach step %s", want)
	}
}

// 投影器正常完成：finalizer 不回源、不隔离实体、不重复发布快照；投影器发布后立即收尾释放。
func TestAsyncFinalizerLeavesProjectionToProjector(t *testing.T) {
	f := newProjectionOwnershipFixture(t, 1891)
	tx := remoteTestTxID(191)
	commits := f.asyncWrite(t, tx)
	first := f.nextStep(t, "projection pending")
	if first != "await_projection" {
		t.Fatalf("first finalizer step=%s, want await_projection", first)
	}
	if got := f.live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("entity state while projection pending=%v, want local_owned (not quarantined by the finalizer)", got)
	}
	// WAL 投影器：同一 Mongo 事务提交后发布。
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatalf("projector publication: %v", err)
	}
	if step := f.nextStep(t, "after projector publication"); step != "released" {
		t.Fatalf("finalizer step after publication=%s, want released", step)
	}
	if reads := f.storage.statusCalls.Load(); reads != 0 {
		t.Fatalf("finalizer backend status reads=%d, want 0", reads)
	}
	if published := f.syncer.published.Load(); published != 1 {
		t.Fatalf("snapshot publications=%d, want 1 (projector only)", published)
	}
	if got := f.live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("entity state after completion=%v, want local_owned", got)
	}
	if slots := len(f.mgr.remote.writeSlots); slots != 0 {
		t.Fatalf("write slots=%d after completion", slots)
	}
	if status, err := f.mgr.RemoteCommitStatus(context.Background(), tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("status=%v err=%v, want committed", status.State, err)
	}
}

// 原双发布者交错已经取消。保留未知结果后的恢复压力：唯一发布者暂停时，
// finalizer 只能等待；幂等重放和显式恢复不能绕过它再发布。
func TestOutboxOwnsPublicationAfterIndeterminateProjection(t *testing.T) {
	f := newProjectionOwnershipFixture(t, 1892)
	tx := remoteTestTxID(192)
	commits := f.asyncWrite(t, tx)
	f.projectorReportsUnknown(t, tx, commits)
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	f.live.armed.Store(true)
	release := sync.OnceFunc(func() { close(f.live.resume) })
	t.Cleanup(release)
	f.answerPoll(t, true)
	select {
	case <-f.live.reached:
	case <-time.After(3 * time.Second):
		t.Fatal("outbox never entered acknowledgement")
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatal(err)
	}
	if got := f.syncer.published.Load(); got != 0 {
		t.Fatalf("another caller published while outbox was paused: %d", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.mgr.RecoverOutbox(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent recovery bypassed publisher: %v", err)
	}
	release()
	f.finishPublication(t)
	if got := f.syncer.published.Load(); got != 1 {
		t.Fatalf("publications=%d want 1", got)
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.RecoverOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.syncer.published.Load(); got != 1 {
		t.Fatalf("committed replay republished: %d", got)
	}
}

// 补发失败期间 finalizer 可以报告 retry；恢复后仍只释放一次，不能把 retry 次数当失败。
func (f projectionOwnershipFixture) finishPublication(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-f.storage.polled:
			f.storage.answer <- true
		case step := <-f.steps:
			if step == "released" {
				if len(f.mgr.remote.writeSlots) != 0 || f.live.testRemoteEntity.RemoteOwnershipState() != entity.RemoteOwnershipLocalOwned {
					t.Fatal("publication did not release and restore the local entity")
				}
				return
			}
			if step != "retry" && step != "await_projection" {
				t.Fatalf("unexpected finalizer step %s", step)
			}
		case <-timer.C:
			t.Fatal("publication did not converge")
		}
	}
}

// projectorReportsUnknown 让投影器的第一次发布以瞬时错误结束（tracker Indeterminate），
// finalizer 被唤醒后回源读到 Unknown，隔离实体并进入重试。
func (f projectionOwnershipFixture) projectorReportsUnknown(t *testing.T, tx entity.RemoteTransactionID, commits []entity.RemoteCommit) {
	t.Helper()
	select {
	case step := <-f.steps:
		if step != "await_projection" {
			t.Fatalf("first finalizer step=%s, want await_projection", step)
		}
	case <-f.storage.polled:
		// 修复前：finalizer 立即回源。按 Unknown 应答，让旧实现走到同样的“隔离 + 重试”状态。
		f.storage.answer <- false
		f.awaitStep(t, "retry")
	case <-time.After(3 * time.Second):
		t.Fatal("finalizer made no progress")
	}
	f.storage.failNextCommit.Store(true)
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("injected projector failure err=%v, want indeterminate", err)
	}
	f.answerPoll(t, false)
	f.awaitStep(t, "retry")
	if got := f.live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipQuarantined {
		t.Fatalf("entity state after unknown=%v, want quarantined", got)
	}
}

// 提交路径在 WAL 准入之前（FinalizeLocked）拒绝带 dbscope=sid 的 mutation / delete：装配期按注册的 DaoBuilders 校验看不到
// 手写实体实际持有的 sid DAO。错误可 errors.Is ErrRemoteManagedServerScopedDAO，已定稿的条目回滚、批次不产生任何提交；
// 投影 / 重放路径不拒绝。
// 历史：RR-20260927-09。

// serverScopedCommitEntity 的注册 DAO 工厂不声明 DbScope（装配校验放行），实际提交却带 dbscope=sid。
type serverScopedCommitEntity struct {
	*testRemoteEntity
}

func (e *serverScopedCommitEntity) BuildRemoteCommitLocked(lease entity.RemoteWriteLease, outcome entity.RemoteTransactionOutcome) (entity.RemoteCommit, error) {
	commit, err := e.testRemoteEntity.BuildRemoteCommitLocked(lease, outcome)
	for i := range commit.Mutations {
		commit.Mutations[i].DatabaseScope = uint8(entity.DatabaseServer)
	}
	for i := range commit.Deletes {
		commit.Deletes[i].DatabaseScope = uint8(entity.DatabaseServer)
	}
	return commit, err
}

func TestFinalizeLockedRejectsServerScopedRemoteData(t *testing.T) {
	for i, deleteCommit := range []bool{false, true} {
		name := map[bool]string{false: "update", true: "delete"}[deleteCommit]
		t.Run(name, func(t *testing.T) {
			const kind entity.EntityKind = 143
			entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
			mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
			loader := newRemoteTestLoader()
			mgr.SetBackend(loader)
			mgr.SetOwnershipStore(newMockMarkerStore())
			live := &serverScopedCommitEntity{newTestRemoteEntity(2431+int64(i), 1, kind)}
			live.dirty.set(true)
			loader.add(live)

			ctx := context.Background()
			batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
			if err != nil {
				t.Fatal(err)
			}
			outcome := entity.NewRemoteTransactionOutcome(remoteTestTxID(byte(43+i)), "sid", "", true, 2)
			if deleteCommit {
				outcome.DeleteIntents = batchDeleteIntent(live.GUId())
			}
			err = batch.FinalizeLocked(outcome)
			if err == nil {
				t.Fatalf("FinalizeLocked accepted a remote-managed %s commit whose data uses dbscope=sid; commits=%+v", name, batch.Commits())
			}
			if !errors.Is(err, ErrRemoteManagedServerScopedDAO) || !errors.Is(err, entity.ErrRemoteManagedServerScopedDAO) {
				t.Fatalf("FinalizeLocked = %v, want errors.Is ErrRemoteManagedServerScopedDAO", err)
			}
			if commits := batch.Commits(); len(commits) != 0 {
				t.Fatalf("a rejected FinalizeLocked left commits for the WAL: %+v", commits)
			}
			if err := batch.Abort(ctx, errors.New("test complete")); err != nil {
				t.Fatal(err)
			}
			if err := batch.Close(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
