package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
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
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
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
