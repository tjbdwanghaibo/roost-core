package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

// RR-20260926-30：DataEngine 驱逐被跳过的原生步骤留下的实体时，要在快池持锁执行，调用方是
// 框架后台 goroutine。NestMgr.RunLocal 是这条正式入口：fn 在快 worker 上运行、调用方同步等它；
// 快 worker 上调用 fail-fast；未运行的引擎拒绝且不执行 fn；它不排在同 ID 链上，所以同一实体
// 正在慢准备（例如等待该实体投影/驱逐的冷加载）时也能完成，不会互相等待。

type binderCommitter struct {
	run func(func()) error
}

func (*binderCommitter) Commit(context.Context, CommitRecord) error { return nil }
func (committer *binderCommitter) BindLocalExecutor(run func(func()) error) {
	committer.run = run
}

func TestRunLocalExecutesOnTheFastPoolAndIsBoundToTheCommitter(t *testing.T) {
	committer := &binderCommitter{}
	mgr := NewEngine(NestOptionWithGetter(newMockGetter()), NestOptionWithTransactionCommitter(committer))
	if committer.run == nil {
		t.Fatal("NewEngine did not hand RunLocal to a LocalExecutorBinder committer")
	}
	ran := false
	if err := committer.run(func() { ran = true }); !errors.Is(err, ErrNestStopped) || ran {
		t.Fatalf("RunLocal before Start=%v ran=%v, want ErrNestStopped without running", err, ran)
	}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	onFast := false
	if err := committer.run(func() { onFast = fctx.InFastWorker() }); err != nil || !onFast {
		t.Fatalf("RunLocal=%v on fast worker=%v", err, onFast)
	}
	if err := mgr.RunLocal(context.Background(), func() { panic("injected") }); err == nil {
		t.Fatal("a panicking task reported success")
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ran = false
	if err := committer.run(func() { ran = true }); !errors.Is(err, ErrNestStopped) || ran {
		t.Fatalf("RunLocal after Shutdown=%v ran=%v, want ErrNestStopped without running", err, ran)
	}
}

func TestRunLocalFromAFastWorkerFailsFast(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 9920, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{1, 4}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("self_run_local"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return nil, mgr.RunLocal(context.Background(), func() {})
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	if _, err := mgr.Request(context.Background(), NewHandlerName("self_run_local"), id, nil); !errors.Is(err, fctx.ErrBlockingInFastWorker) {
		t.Fatalf("RunLocal on a fast worker=%v, want ErrBlockingInFastWorker", err)
	}
}

func TestRunLocalIsNotOrderedBehindTheEntitysSlowPreparation(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 9921, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	slow := &slowTestGetter{Getter: getter, entered: make(chan struct{}), release: make(chan struct{})}
	mgr := NewEngine(NestOptionWithGetter(slow), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{1, 4}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("cold"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name, msg.Tid, msg.Cost = "cold", id, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, slow.entered) // 同一实体的消息正卡在慢准备里
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.RunLocal(context.Background(), func() {}); err != nil {
			t.Error(err)
		}
	}()
	stagedSignal(t, done)
	close(slow.release)
	if got := stagedWait(t, ch); got != "ok" {
		t.Fatal(got)
	}
}
