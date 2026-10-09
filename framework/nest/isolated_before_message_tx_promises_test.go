package nest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// OPEN-ITEMS B19：RR-20260926-84 的判据是入口（只有 invokeHandlerTransaction 认领消息），不看时序。消息自己的事务开始之前——慢阶段准备
// 里——调用 RunIsolatedTransaction 同样按嵌套独立事务处理：带 Remote 批次的消息返回 ErrNestedTransactionInRemoteMessage、函数体不执行、
// 批次不受影响；纯本地消息照常执行但不认领消息（外层失败时回复带 ErrNestedTransactionCommitted、不带 ErrAfterCommitFailed，也不重排）。
// 慢阶段准备的业务可见入口是 prepareSlowEntities 里对 Getter 的读取（加载 / 构建回调在这里运行），测试用包一层的 Getter 在那里调用。

type prepareHookGetter struct {
	*mockGetter
	hook func()
}

func (g *prepareHookGetter) Get(ctx context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	g.hook()
	return g.mockGetter.Get(ctx, id, category)
}

func (g *prepareHookGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	g.hook()
	return g.mockGetter.GetMany(ctx, ids, categories)
}

// isolatedInPrepare 记录慢阶段准备里那一次 RunIsolatedTransaction 的观察结果（只在第一次读取时调用）。
type isolatedInPrepare struct {
	once              atomic.Bool
	inMessage, inFast bool
	callRan           bool
	err               error
}

func (o *isolatedInPrepare) run(iso TransactionCommitter, target int64) {
	if !o.once.CompareAndSwap(false, true) {
		return
	}
	o.inMessage, o.inFast = currentNestDispatchMsg() != nil, fctx.InFastWorker()
	_, o.err = RunIsolatedTransaction(context.Background(), iso, "b19_iso_in_prepare", func() (any, error) {
		o.callRan = true
		return nil, CurrentRollbackTx().AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: target, Database: "test", Resource: "b19_prepare"}, Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: []byte(`{}`)})
	})
}

func TestIsolatedInSlowPrepareOfLocalMessageDoesNotClaimIt(t *testing.T) {
	id := mustBuildCastID(t, 48600, entity.EntityCategory(1), nestLocalKind)
	obs := &isolatedInPrepare{}
	iso := &recordsCommitter{}
	getter := &prepareHookGetter{mockGetter: newMockGetter(), hook: func() { obs.run(iso, id) }}
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	outer := &recordsCommitter{}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{1, 8}))
	name := NewHandlerName("b19_local_after_prepare")
	var attempts atomic.Int64
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		attempts.Add(1)
		return nil, fmt.Errorf("%w: outer failed after the isolated commit in prepare", ErrLockTimeout)
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := mgr.Request(ctx, name, id, nil, SendOptionSlow())

	if !obs.inMessage || obs.inFast {
		t.Fatalf("premise: the isolated call must run in the message's slow stage (in message=%v, fast worker=%v)", obs.inMessage, obs.inFast)
	}
	if obs.err != nil || !obs.callRan || iso.count() != 1 {
		t.Fatalf("local message: isolated err=%v ran=%v commits=%d, want it to run and commit once", obs.err, obs.callRan, iso.count())
	}
	if errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("reply=%v: the isolated transaction in prepare claimed the message (reply reports the message's own commit)", err)
	}
	if !errors.Is(err, ErrNestedTransactionCommitted) || !errors.Is(err, ErrLockTimeout) || attempts.Load() != 1 {
		t.Fatalf("reply=%v attempts=%d: want ErrNestedTransactionCommitted around the outer error and no requeue (RR-65)", err, attempts.Load())
	}
	if outer.count() != 0 {
		t.Fatalf("outer committer got %d record(s) although the handler failed", outer.count())
	}
}

func TestIsolatedInSlowPrepareOfRemoteMessageIsRefused(t *testing.T) {
	localID, local := newAsyncPilotEntity(t, 48610, 1)
	remoteID := mustBuildCastID(t, 48611, entity.EntityCategoryRemote, nestRemoteManagedKind)
	obs := &isolatedInPrepare{}
	iso := &recordsCommitter{}
	getter := &prepareHookGetter{mockGetter: newMockGetter(), hook: func() { obs.run(iso, localID) }}
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(local)
	var batchCommits atomic.Int32
	batch := &stagedRemoteBatch{commit: func() { batchCommits.Add(1) }}
	remote := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(remote), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("b19_remote_after_prepare")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	var reply any
	select {
	case reply = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("Remote message did not reply within 10s")
	}

	if !obs.inMessage || obs.inFast {
		t.Fatalf("premise: the isolated call must run in the message's slow stage (in message=%v, fast worker=%v)", obs.inMessage, obs.inFast)
	}
	if !errors.Is(obs.err, ErrNestedTransactionInRemoteMessage) || obs.callRan || iso.count() != 0 {
		t.Fatalf("Remote message: isolated err=%v ran=%v commits=%d, want ErrNestedTransactionInRemoteMessage with the body not executed", obs.err, obs.callRan, iso.count())
	}
	if reply != "ok" || batchCommits.Load() != 1 || batch.aborted.Load() {
		t.Fatalf("reply=%v batch commits=%d aborted=%v: the refused isolated call must not touch the message or its batch", reply, batchCommits.Load(), batch.aborted.Load())
	}
}
