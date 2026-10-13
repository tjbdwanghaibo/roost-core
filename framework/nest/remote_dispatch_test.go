// Remote 消息调度：I/O 阶段准备与业务续行的顺序和停机排空、预算拒绝、本地执行入口、Close 失败与拒绝后的 Sync、远端快照访问。

package nest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

type stagedRemoteManager struct {
	entity.IRemoteEntityManager
	prepare func(context.Context) (entity.RemoteWriteBatch, error)
}

func (m stagedRemoteManager) PrepareRemoteWriteBatch(ctx context.Context, _ []int64) (entity.RemoteWriteBatch, error) {
	return m.prepare(ctx)
}

type stagedRemoteBatch struct {
	entity.RemoteWriteBatch
	commit  func()
	close   func()
	aborted atomic.Bool
	closed  atomic.Int32
}

func (b *stagedRemoteBatch) FinalizeLocked(entity.RemoteTransactionOutcome) error { return nil }
func (b *stagedRemoteBatch) Commits() []entity.RemoteCommit                       { return nil }
func (b *stagedRemoteBatch) Commit(context.Context) ([]entity.RemoteCommitReceipt, error) {
	if b.commit != nil {
		b.commit()
	}
	return nil, nil
}
func (b *stagedRemoteBatch) Abort(context.Context, error) error { b.aborted.Store(true); return nil }
func (b *stagedRemoteBatch) Close(context.Context) error {
	if b.close != nil {
		b.close()
	}
	b.closed.Add(1)
	return nil
}

func stagedEngine(t *testing.T, manager entity.IRemoteEntityManager) (*NestMgr, int64, int64) {
	t.Helper()
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 9011, entity.EntityCategoryRemote, nestRemoteManagedKind)
	localID := mustBuildCastID(t, 9012, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(newMockEntity(localID, entity.EntityCategory(1)))
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("staged"), func(_ []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
		if len(params) > 0 {
			if fn, ok := params[0].(func()); ok {
				fn()
			}
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mgr.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return mgr, remoteID, localID
}
func stagedSend(t *testing.T, mgr *NestMgr, id int64, remote bool, params ...any) <-chan any {
	t.Helper()
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name = "staged"
	msg.Tid = id
	msg.Cost = remote
	msg.HasRemote = remote
	msg.Params = params
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	return ch
}
func stagedWait(t *testing.T, ch <-chan any) any {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("blocked behind Remote I/O")
		return nil
	}
}
func stagedSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("stage did not start")
	}
}

func TestRemoteStagesDoNotBlockCostLogicWorker(t *testing.T) {
	for _, stage := range []string{"prepare", "commit", "close"} {
		t.Run(stage, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			block := func() { close(entered); <-release }
			batch := &stagedRemoteBatch{}
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
				if stage == "prepare" {
					block()
				}
				return batch, nil
			}}
			if stage == "commit" {
				batch.commit = block
			}
			if stage == "close" {
				batch.close = block
			}
			mgr, remoteID, localID := stagedEngine(t, manager)
			defer close(release)
			remoteReply := stagedSend(t, mgr, remoteID, true)
			stagedSignal(t, entered)
			if got := stagedWait(t, stagedSend(t, mgr, localID, false)); got != "ok" {
				t.Fatalf("local reply=%v", got)
			}
			select {
			case reply := <-remoteReply:
				t.Fatalf("replied before %s finished: %v", stage, reply)
			default:
			}
		})
	}
}

func TestRemoteStagesPreserveOrderAndDrainBeforeShutdown(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	batch := &stagedRemoteBatch{close: func() { close(entered); <-release }}
	var prepares atomic.Int32
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
		if prepares.Add(1) == 1 {
			return batch, nil
		}
		return &stagedRemoteBatch{}, nil
	}}
	mgr, id, _ := stagedEngine(t, manager)
	first := stagedSend(t, mgr, id, true)
	stagedSignal(t, entered)
	second := stagedSend(t, mgr, id, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mgr.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown=%v", err)
	}
	if got := prepares.Load(); got != 1 {
		t.Fatalf("next same-key request overtook release: %d", got)
	}
	close(release)
	if got := stagedWait(t, first); got != "ok" {
		t.Fatal(got)
	}
	if got := stagedWait(t, second); got != "ok" {
		t.Fatal(got)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if batch.closed.Load() != 1 {
		t.Fatal("batch close count")
	}
}

func TestRemoteStagesCancelAndFenceAfterPrepareCloseBatch(t *testing.T) {
	for _, scenario := range []string{"cancel", "fence", "panic"} {
		t.Run(scenario, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			batch := &stagedRemoteBatch{}
			mgr, id, _ := stagedEngine(t, stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { close(entered); <-release; return batch, nil }})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			msg, ch := GenSyncMsg(MsgTypeSingle)
			msg.Name = "staged"
			msg.Tid = id
			msg.HasRemote = true
			msg.Context = fctx.ContextSnapshot{Valid: true, Base: ctx}
			var ran atomic.Bool
			msg.Params = []any{func() {
				ran.Store(true)
				if scenario == "panic" {
					panic("business panic")
				}
			}}
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			stagedSignal(t, entered)
			switch scenario {
			case "cancel":
				cancel()
			case "fence":
				mgr.Fence(errors.New("fenced test"))
			}
			close(release)
			if _, ok := stagedWait(t, ch).(error); !ok {
				t.Fatal("missing error")
			}
			if !batch.aborted.Load() || batch.closed.Load() != 1 {
				t.Fatal("batch not aborted and closed once")
			}
			if scenario != "panic" && ran.Load() {
				t.Fatal("handler ran after cancellation/fence")
			}
		})
	}
}

func TestRemoteStagesFullFastQueueReservesContinuation(t *testing.T) {
	batch := &stagedRemoteBatch{}
	prepared := make(chan struct{})
	mgr, remoteID, localID := stagedEngine(t, stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { close(prepared); return batch, nil }})
	entered, release := make(chan struct{}), make(chan struct{})
	stagedSend(t, mgr, localID, false, func() { close(entered); <-release })
	stagedSignal(t, entered)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	for range mgr.dispatcher.MsgCap {
		stagedSend(t, mgr, localID, false)
	}
	reply := stagedSend(t, mgr, remoteID, true)
	stagedSignal(t, prepared)
	close(release)
	if got := stagedWait(t, reply); got != "ok" {
		t.Fatalf("reserved continuation failed: %v", got)
	}
	if batch.aborted.Load() || batch.closed.Load() != 1 {
		t.Fatal("prepared batch did not complete once")
	}

}

func TestRemoteStagesReturnContextAndConfirmAfterGuardRelease(t *testing.T) {
	type contextKey struct{}
	batch := &stagedRemoteBatch{}
	mgr, id, _ := stagedEngine(t, stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }})
	var confirmed atomic.Bool
	reply := stagedSend(t, mgr, id, true, func() {
		if entity.CurrentGuardScope() == nil {
			panic("handler has no guard")
		}
		fctx.CurrentContext().Set(contextKey{}, "from handler")
		currentNestDispatchMsg().addPostRemoteCommit(func() {
			if entity.CurrentGuardScope() != nil {
				panic("confirmation still owns guard")
			}
			if got, ok := fctx.CurrentContext().Get(contextKey{}); !ok || got != "from handler" {
				panic("lost handler context")
			}
			confirmed.Store(true)
		})
	})
	if got := stagedWait(t, reply); got != "ok" {
		t.Fatal(got)
	}
	if !confirmed.Load() {
		t.Fatal("confirmation skipped")
	}
}

// Remote 后端预算拒绝发生于I/O 阶段；不得执行业务，也不得遗留同 ID 的调度所有权。
func TestRemoteBudgetRejectionReleasesIDAndWorkers(t *testing.T) {
	var prepares, ran atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	defer closeRelease()
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
		if prepares.Add(1) == 1 {
			close(entered)
			<-release
			return nil, entity.ErrRemoteOverloaded
		}
		return &stagedRemoteBatch{}, nil
	}}
	m, id, local := stagedEngine(t, manager)
	first := stagedSend(t, m, id, true, func() { ran.Add(1) })
	stagedSignal(t, entered)
	second := stagedSend(t, m, id, true, func() { ran.Add(1) })
	// 普通慢业务也能回到业务池执行，不需要获取 Remote 写额度。
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name = "staged"
	msg.Tid = local
	msg.Cost = true
	if err := m.dispatcher.TrySendMsg(msg); err != nil {
		closeRelease()
		t.Fatal(err)
	}
	localReply := stagedWait(t, ch)
	closeRelease()
	if localReply != "ok" {
		t.Fatalf("ordinary slow work=%v", localReply)
	}
	if err, ok := stagedWait(t, first).(error); !ok || !errors.Is(err, entity.ErrRemoteOverloaded) {
		t.Fatalf("refusal=%v", err)
	}
	if got := stagedWait(t, second); got != "ok" {
		t.Fatalf("same ID successor=%v", got)
	}
	if ran.Load() != 1 || prepares.Load() != 2 {
		t.Fatalf("ran=%d prepares=%d", ran.Load(), prepares.Load())
	}
}

// bindingRemoteManager 记录 NewEngine 交给 Remote Manager 的本地执行入口（LocalExecutorBinder）。
type bindingRemoteManager struct {
	stagedRemoteManager
	mu  sync.Mutex
	run func(func()) error
}

func (m *bindingRemoteManager) BindLocalExecutor(run func(func()) error) {
	m.mu.Lock()
	m.run = run
	m.mu.Unlock()
}

func (m *bindingRemoteManager) localExecutor() func(func()) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.run
}

// Remote 后台收尾不属于任何 Nest 消息：NewEngine 把 RunLocal 交给 Remote Manager，从非业务 worker 调用时在业务池执行并
// 等待，panic 作为错误返回；停机后拒绝投递、不执行 fn。
func TestNestBindsRunLocalIntoRemoteManager(t *testing.T) {
	manager := &bindingRemoteManager{}
	mgr := NewEngine(NestOptionWithGetter(newMockGetter()), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	run := manager.localExecutor()
	if run == nil {
		t.Fatal("NewEngine did not bind its RunLocal into the remote manager")
	}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	onFast := make(chan bool, 1)
	done := make(chan error, 1)
	go func() { done <- run(func() { onFast <- fctx.InShortWorker() }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local step did not run")
	}
	if !<-onFast {
		t.Fatal("local step did not run on a business worker")
	}
	if err := run(func() { panic(errors.New("boom")) }); err == nil {
		t.Fatal("panic in a local step was not reported")
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := run(func() { ran = true }); !errors.Is(err, ErrNestStopped) || ran {
		t.Fatalf("local step after shutdown: err=%v ran=%v, want ErrNestStopped without running", err, ran)
	}
}

type closeFailBatch struct{ stagedRemoteBatch }

func (b *closeFailBatch) Close(context.Context) error {
	b.closed.Add(1)
	return entity.ErrRemoteReleaseIncomplete
}

func TestRemoteCloseFailureStillConfirmsEntitySync(t *testing.T) {
	for _, mode := range []entitysync.SyncMode{entitysync.ModePeriodic, entitysync.ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, 9950, 10)
			m, frames, _ := syncTestManager(t, e, mode)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, 9951, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			fail := true
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
				if fail {
					return &closeFailBatch{}, nil
				}
				return &stagedRemoteBatch{}, nil
			}}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithEntitySync(m), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("remote_close_confirmation")
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				e.dao.Value++
				e.MarkSyncDirty(1)
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			send := func() any {
				msg, ch := GenSyncMsg(MsgTypeMulti)
				msg.Name = name.String()
				msg.Tids = []int64{remoteID, localID}
				msg.Cost = true
				msg.HasRemote = true
				if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
					t.Fatal(err)
				}
				return stagedWait(t, ch)
			}
			got := send()
			t.Logf("first reply (commit ok, close failed): %v", got)
			if !e.Sync().SyncCommitReady() {
				t.Fatal("committed transaction left Sync held after Close failure")
			}
			fail = false
			got = send()
			t.Logf("second reply (all ok): %v", got)
			_ = m.Flush(context.Background())
			t.Logf("after successful second tx: SyncCommitReady=%v value=%d", e.Sync().SyncCommitReady(), e.dao.Value)
			select {
			case raw := <-frames:
				t.Logf("frame delivered value=%d", syncValue(t, raw))
			case <-time.After(300 * time.Millisecond):
				t.Errorf("no frame delivered after a later successful transaction: entity sync frozen")
			}
		})
	}
}

// 本地 + Remote 混合事务：本地部分已持久提交、Remote 确认无结论，之后权威持久拒绝 Remote 部分。提交后工作只丢弃批次内
// Remote 实体的 Sync 事实与内容；本地实体的提交事实与冻结内容照常生效；AfterCommit 不执行（结论为拒绝）。
// 历史：RR-20260926-58。
func TestRemoteRejectKeepsCommittedLocalEntitySync(t *testing.T) {
	for _, mode := range []entitysync.SyncMode{entitysync.ModePeriodic, entitysync.ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			unique := int64(9940)
			if mode == entitysync.ModeOnChange {
				unique = 9950
			}
			localID, local := newAsyncPilotEntity(t, unique, 10)
			m, frames, _ := syncTestManager(t, local, mode)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
			remote := newMockEntity(remoteID, entity.EntityCategoryRemote)
			packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.CopyFrozenSyncPayload(1, []byte{0xEE}), nil
			}
			remote.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: remoteID, Namespace: "test", Packer: entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}})
			getter.Add(remote)
			getter.Add(local)
			batch := &outcomeDeferringBatch{ids: []int64{remoteID}}
			manager := &bindingRemoteManager{stagedRemoteManager: stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
				return batch, nil
			}}}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithEntitySync(m), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName(fmt.Sprintf("mixed_remote_reject_%d", unique))
			var localFacts, remoteFacts func() (bool, bool)
			afterCommit := 0
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				local.dao.Value++
				local.MarkSyncDirty(1)
				remote.MarkSyncDirty(1)
				// 与 Interest 事实队列相同的入口：锁内固定本提交的条件。
				localFacts = entity.SyncConditionFor(local)
				remoteFacts = entity.SyncConditionFor(remote)
				AfterCommit(func() { afterCommit++ })
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name = name.String()
			msg.Tids = []int64{remoteID, localID}
			msg.Cost = true
			msg.HasRemote = true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			if reply, ok := stagedWait(t, ch).(error); !ok || reply == nil {
				t.Fatalf("reply=%v, want the unknown remote confirmation", reply)
			}
			outcome := batch.takeOutcome()
			if outcome == nil {
				t.Fatal("post-commit work was not handed to the remote outcome")
			}
			run := manager.localExecutor()
			done := make(chan error, 1)
			go func() { done <- run(func() { outcome(false) }) }()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if ready, discarded := localFacts(); !ready || discarded {
				t.Fatalf("durably committed local entity %d: facts ready=%v discarded=%v after the Remote part was rejected, want ready (only the rejected Remote entity's facts may be dropped)", localID, ready, discarded)
			}
			if ready, discarded := remoteFacts(); ready || !discarded {
				t.Fatalf("rejected Remote entity %d: facts ready=%v discarded=%v, want discarded", remoteID, ready, discarded)
			}
			if !local.Sync().SyncCommitReady() || !remote.Sync().SyncCommitReady() {
				t.Fatalf("gates still frozen after the rejection: local=%v remote=%v", local.Sync().SyncCommitReady(), remote.Sync().SyncCommitReady())
			}
			if afterCommit != 0 {
				t.Fatalf("AfterCommit ran %d time(s) for a rejected transaction", afterCommit)
			}
			// 本地实体已提交的内容（11）直接交付，不需要等下一笔提交。
			deadline := time.After(2 * time.Second)
			for delivered := false; !delivered; {
				_ = m.Flush(context.Background())
				select {
				case raw := <-frames:
					delivered = syncValue(t, raw) == 11
				case <-deadline:
					t.Fatal("committed local content 11 never delivered after the Remote rejection")
				}
			}
		})
	}
}

type testRemoteAccessRequest struct {
	Ref entity.RemoteViewRef
}

func (r testRemoteAccessRequest) RemoteAccess() []RemoteAccess {
	return []RemoteAccess{
		{
			Alias: "target_player",
			Ref:   r.Ref,
			Mode:  RemoteAcquireCache,
			Scope: 7,
		},
	}
}

type testRemoteSnapshotResolver struct {
	calls []RemoteAccess
}

func (r *testRemoteSnapshotResolver) ResolveRemoteSnapshot(access RemoteAccess) (entity.RemoteSnapshot, error) {
	r.calls = append(r.calls, access)
	version := uint64(22)
	if access.MinVersion > version {
		version = access.MinVersion
	}
	return entity.RemoteSnapshot{
		EntityID:   access.Ref.EntityID,
		Kind:       access.Ref.Kind,
		Scope:      access.Scope,
		Version:    version,
		RouteEpoch: access.Ref.RouteEpoch,
		Data:       "cached-view",
	}, nil
}

func TestNestRemoteAccessPreloadsSnapshotBeforeHandler(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	id := mustBuildCastID(t, 7101, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.Add(e)

	refID := mustBuildCastID(t, 7201, entity.EntityCategoryRemote, nestRemoteManagedKind)
	ref := entity.RemoteViewRef{EntityID: refID, Kind: nestRemoteManagedKind, Version: 20}
	resolver := &testRemoteSnapshotResolver{}

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithRemoteSnapshotResolver(resolver),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	name := NewHandlerName("test_remote_access_preload")
	key := RemoteKey[string]{Alias: "target_player"}
	MustRegisterMemoryHandler(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		snapshot, ok := Remote(key)
		if !ok {
			return nil, errors.New("missing target_player snapshot")
		}
		return snapshot, nil
	})

	ret, err := Nest.Request(context.Background(), name, id, NewParams(testRemoteAccessRequest{Ref: ref}))
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if ret != "cached-view" {
		t.Fatalf("remote snapshot = %v, want cached-view", ret)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1", len(resolver.calls))
	}
	if resolver.calls[0].Alias != "target_player" || resolver.calls[0].Mode != RemoteAcquireCache {
		t.Fatalf("resolver call = %+v", resolver.calls[0])
	}
}

type testRemoteAccessWithTTLRequest struct {
	Ref entity.RemoteViewRef
}

func (r testRemoteAccessWithTTLRequest) RemoteAccess() []RemoteAccess {
	return []RemoteAccess{
		{
			Alias:          "target_player",
			Ref:            r.Ref,
			Mode:           RemoteAcquireCache,
			Scope:          7,
			MinVersion:     r.Ref.Version,
			CacheTTLMillis: 30000,
			Required:       true,
		},
	}
}

func TestNestRemoteKeyAndRemoteAccessTTL(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	id := mustBuildCastID(t, 7301, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.Add(e)

	refID := mustBuildCastID(t, 7302, entity.EntityCategoryRemote, nestRemoteManagedKind)
	ref := entity.RemoteViewRef{EntityID: refID, Kind: nestRemoteManagedKind, Version: 31}
	resolver := &testRemoteSnapshotResolver{}

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithRemoteSnapshotResolver(resolver),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	key := RemoteKey[string]{Alias: "target_player"}
	name := NewHandlerName("test_remote_key_ttl")
	MustRegisterMemoryHandler(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return MustRemote(key), nil
	})

	ret, err := Nest.Request(context.Background(), name, id, NewParams(testRemoteAccessWithTTLRequest{Ref: ref}))
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if ret != "cached-view" {
		t.Fatalf("remote snapshot = %v, want cached-view", ret)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1", len(resolver.calls))
	}
	if resolver.calls[0].MinVersion != 31 || resolver.calls[0].CacheTTLMillis != 30000 || !resolver.calls[0].Required {
		t.Fatalf("resolver call remote policy = %+v", resolver.calls[0])
	}
}

func TestShouldPrepareRemoteIDUsesRemotePolicyByKind(t *testing.T) {
	// The remote kinds are registered in EntityCategoryRemote; nestUnknownKind
	// is deliberately never registered, so it may take any category here.
	const localCategory = entity.EntityCategoryRemote + 2
	remoteManagedID := mustBuildCastID(t, 1, entity.EntityCategoryRemote, nestRemoteManagedKind)
	if !shouldPrepareRemoteID(entity.ResolveEntityID(remoteManagedID)) {
		t.Fatal("remote-managed id should prepare remote entity")
	}

	localManagedID := int64(uint64(remoteManagedID) & ^(entity.EntityRemoteMask << entity.EntityRemoteShift))
	if !shouldPrepareRemoteID(entity.ResolveEntityID(localManagedID)) {
		t.Fatal("remote-managed kind should prepare even when an input id missed the remote bit")
	}

	remoteCapableID := mustBuildCastID(t, 1, entity.EntityCategoryRemote, nestRemoteCapableKind)
	if shouldPrepareRemoteID(entity.ResolveEntityID(remoteCapableID)) {
		t.Fatal("remote-capable but unmanaged kind should not use remote prepare")
	}

	remoteUnknownKindID := int64(uint64(mustBuildCastID(t, 1, localCategory, nestUnknownKind)) | (entity.EntityRemoteMask << entity.EntityRemoteShift))
	if shouldPrepareRemoteID(entity.ResolveEntityID(remoteUnknownKindID)) {
		t.Fatal("remote bit without remote-managed kind should not prepare remote entity")
	}

	categoryOnlyID := mustBuildCastID(t, 1, localCategory, entity.EntityKindNone)
	if shouldPrepareRemoteID(entity.ResolveEntityID(categoryOnlyID)) {
		t.Fatal("category must not imply remote entity")
	}
}

func TestEntityKindRemoteCapability(t *testing.T) {
	if !entity.IsEntityKindRemoteCapable(nestRemoteCapableKind) {
		t.Fatal("remote=mirror kind should be remote-capable")
	}
	if !entity.IsEntityKindRemoteCapable(nestRemoteManagedKind) {
		t.Fatal("remote=managed kind should be remote-capable")
	}
	if entity.IsEntityKindRemoteCapable(nestUnknownKind) {
		t.Fatal("unregistered kind should not be remote-capable")
	}
}
