// Pipelined 异步完成：持久等待不占业务 worker，同实体完成顺序、完成池饱和降级、释放失败仍回复且只回复一次。

package nest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

func newAsyncPilotEntity(t *testing.T, unique int64, value int) (int64, *rollbackTestEntity) {
	t.Helper()
	id := mustBuildCastID(t, unique, entity.EntityCategory(1), nestLocalKind)
	return id, &rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        &rollbackTestDao{id: id, Value: value},
	}
}

func registerAsyncIncrementHandler(name string, hooks *[]string, hooksMu *sync.Mutex, tag string) {
	MustRegisterHandlerWithMeta(NewHandlerName(name), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		e := es[0].(*rollbackTestEntity)
		old := e.dao.Value
		if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value++
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		if hooks != nil {
			AfterCommit(func() {
				hooksMu.Lock()
				*hooks = append(*hooks, tag)
				hooksMu.Unlock()
			})
		}
		return e.dao.Value, nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
}

func TestAsyncCompletionFreesWorkerDuringDurableWait(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	// The Phase 2 property: with one worker, a pipelined transaction waiting
	// for durability must not block the worker — a request to a different
	// entity on the same worker completes while the first ticket is pending.
	getter := newMockGetter()
	idA, entA := newAsyncPilotEntity(t, 340, 10)
	idB, entB := newAsyncPilotEntity(t, 341, 0)
	getter.Add(entA)
	getter.Add(entB)
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithPipelinedAsyncCompletion(0, 0),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	registerAsyncIncrementHandler("test_async_frees_worker", nil, nil, "")
	MustRegisterMemoryHandler(NewHandlerName("test_async_bystander"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return "bystander", nil
	})

	t1Done := make(chan any, 1)
	go func() {
		ret, err := Nest.Request(context.Background(), NewHandlerName("test_async_frees_worker"), idA, nil)
		if err != nil {
			t1Done <- err
			return
		}
		t1Done <- ret
	}()
	<-committer.enqueued // T1 enqueued; ticket unresolved; worker must be free

	bystander := make(chan any, 1)
	go func() {
		ret, err := Nest.Request(context.Background(), NewHandlerName("test_async_bystander"), idB, nil)
		if err != nil {
			bystander <- err
			return
		}
		bystander <- ret
	}()
	select {
	case ret := <-bystander:
		if ret != "bystander" {
			t.Fatalf("bystander result=%v", ret)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker is still parked on the durability wait (Phase 1 behavior)")
	}
	select {
	case ret := <-t1Done:
		t.Fatalf("pipelined reply escaped before durability: %v", ret)
	case <-time.After(20 * time.Millisecond):
	}

	committer.resolveAll(nil)
	select {
	case ret := <-t1Done:
		if ret != 11 {
			t.Fatalf("pipelined result=%v, want 11", ret)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipelined reply never delivered")
	}
	if entA.Base().LastCommitLSN() != 1 || entA.dao.Value != 11 {
		t.Fatalf("entity state: lsn=%d value=%d", entA.Base().LastCommitLSN(), entA.dao.Value)
	}
	committer.mu.Lock()
	released := !committer.released.IsZero()
	committer.mu.Unlock()
	if !released {
		t.Fatal("TransactionReleased was not delivered by the completion")
	}
}

func TestAsyncCompletionKeepsSameEntityCommitOrder(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	// Two pipelined transactions on one entity: the second handler runs while
	// the first is still waiting for durability (that is the point), but
	// AfterCommit hooks and replies must fire in commit (LSN) order.
	getter := newMockGetter()
	id, ent := newAsyncPilotEntity(t, 342, 0)
	getter.Add(ent)
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithPipelinedAsyncCompletion(0, 0),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	var hooks []string
	var hooksMu sync.Mutex
	registerAsyncIncrementHandler("test_async_order_first", &hooks, &hooksMu, "first")
	registerAsyncIncrementHandler("test_async_order_second", &hooks, &hooksMu, "second")

	firstDone := make(chan any, 1)
	go func() {
		ret, _ := Nest.Request(context.Background(), NewHandlerName("test_async_order_first"), id, nil)
		firstDone <- ret
	}()
	<-committer.enqueued // LSN 1 admitted, lock released, worker free

	secondDone := make(chan any, 1)
	go func() {
		ret, _ := Nest.Request(context.Background(), NewHandlerName("test_async_order_second"), id, nil)
		secondDone <- ret
	}()
	// The second handler must run to completion (enqueue LSN 2) while the
	// first ticket is still unresolved: it reads the first transaction's
	// in-memory state under prefix-durability protection.
	select {
	case <-committer.enqueued:
	case <-time.After(2 * time.Second):
		t.Fatal("second transaction blocked behind the first durability wait")
	}

	committer.resolveAll(nil)
	if ret := <-firstDone; ret != 1 {
		t.Fatalf("first result=%v, want 1", ret)
	}
	if ret := <-secondDone; ret != 2 {
		t.Fatalf("second result=%v, want 2", ret)
	}
	// Hooks may still be in flight on the completion pool after the replies;
	// wait briefly for both.
	deadline := time.Now().Add(2 * time.Second)
	for {
		hooksMu.Lock()
		n := len(hooks)
		hooksMu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	hooksMu.Lock()
	defer hooksMu.Unlock()
	if len(hooks) != 2 || hooks[0] != "first" || hooks[1] != "second" {
		t.Fatalf("AfterCommit order=%v, want [first second]", hooks)
	}
	if ent.dao.Value != 2 || ent.Base().LastCommitLSN() != 2 {
		t.Fatalf("entity state: value=%d lsn=%d", ent.dao.Value, ent.Base().LastCommitLSN())
	}
}

func TestAsyncCompletionIndeterminateRepliesErrorWithoutRollback(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id, ent := newAsyncPilotEntity(t, 343, 10)
	getter.Add(ent)
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithPipelinedAsyncCompletion(0, 0),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	registerAsyncIncrementHandler("test_async_indeterminate", nil, nil, "")
	done := make(chan error, 1)
	go func() {
		_, err := Nest.Request(context.Background(), NewHandlerName("test_async_indeterminate"), id, nil)
		done <- err
	}()
	<-committer.enqueued
	committer.resolveAll(ErrCommitIndeterminate)
	select {
	case err := <-done:
		if !errors.Is(err, ErrCommitIndeterminate) {
			t.Fatalf("err=%v, want %v", err, ErrCommitIndeterminate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("indeterminate verdict never reached the caller")
	}
	if ent.dao.Value != 11 {
		t.Fatalf("value=%d: indeterminate outcome must not roll back", ent.dao.Value)
	}
	if fence := Nest.FenceError(); !errors.Is(fence, ErrNestFenced) || !errors.Is(fence, ErrCommitIndeterminate) {
		t.Fatalf("fence=%v", fence)
	}
}

func TestAsyncCompletionShutdownDeliversPendingReplies(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	// A deferred completion is accepted work: Shutdown must not finish until
	// the pending reply is delivered.
	getter := newMockGetter()
	id, ent := newAsyncPilotEntity(t, 344, 0)
	getter.Add(ent)
	_ = ent
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithPipelinedAsyncCompletion(0, 0),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	stopped := false
	defer func() {
		if !stopped {
			StopNest()
		}
	}()

	registerAsyncIncrementHandler("test_async_shutdown_drain", nil, nil, "")
	replied := make(chan any, 1)
	go func() {
		ret, _ := Nest.Request(context.Background(), NewHandlerName("test_async_shutdown_drain"), id, nil)
		replied <- ret
	}()
	<-committer.enqueued

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- Nest.Shutdown(ctx)
	}()
	// Shutdown must be gated on the unresolved completion.
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown finished before the deferred completion: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	committer.resolveAll(nil)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	stopped = true
	select {
	case ret := <-replied:
		if ret != 1 {
			t.Fatalf("reply=%v, want 1", ret)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending reply was dropped by shutdown")
	}
}

func TestAsyncCompletionKeepsOrderWhenPumpIsSaturated(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	// Regression: the saturated-pump fallback used to run its Commit and
	// AfterCommit hooks inline without regard for same-entity completions
	// still queued in the pump, so an overloaded engine could reorder
	// AfterCommit against commit order. The entity completion chain now
	// covers every path, so ordering is unconditional.
	getter := newMockGetter()
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })
	id, ent := newAsyncPilotEntity(t, 345, 0)
	getter.Add(ent)
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		// Queue capacity 1: the pump holds one entry in flight and one in the
		// queue, so the third transaction is forced onto the fallback path.
		NestOptionWithPipelinedAsyncCompletion(1, 1),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	var hooks []int
	var hooksMu sync.Mutex
	MustRegisterHandlerWithMeta(NewHandlerName("test_async_saturated_order"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		e := es[0].(*rollbackTestEntity)
		old := e.dao.Value
		if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value++
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		// The value carries commit order: hooks must observe 1, 2, 3.
		value := e.dao.Value
		AfterCommit(func() {
			hooksMu.Lock()
			hooks = append(hooks, value)
			hooksMu.Unlock()
		})
		return value, nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})

	const transactions = 3
	replies := make(chan any, transactions)
	for i := 0; i < transactions; i++ {
		go func() {
			ret, err := Nest.Request(context.Background(), NewHandlerName("test_async_saturated_order"), id, nil)
			if err != nil {
				replies <- err
				return
			}
			replies <- ret
		}()
	}
	// All three must reach the WAL before any ticket resolves, so that one of
	// them provably takes the saturated fallback.
	for i := 0; i < transactions; i++ {
		select {
		case <-committer.enqueued:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d transactions were enqueued", i, transactions)
		}
	}
	// Enqueue is reported just before prepareCompletion tries the pump. Wait
	// until the third transaction has actually observed the full queue before
	// resolving tickets; otherwise a fast resolve can drain the queue between
	// those two operations and make this overload test scheduler-dependent.
	degraded := int64(0)
	degradedDeadline := time.Now().Add(2 * time.Second)
	for degraded == 0 && time.Now().Before(degradedDeadline) {
		for _, metric := range metrics.Snapshot() {
			if metric.Name == "nest.pipelined.async_total" && metric.Labels["result"] == "degraded" {
				degraded += metric.Value
			}
		}
		if degraded == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if degraded == 0 {
		committer.resolveAll(errors.New("test cleanup: pump did not saturate"))
		t.Fatal("no transaction took the saturated fallback path: the ordering claim was not exercised")
	}
	committer.resolveAll(nil)

	seen := make(map[int]bool, transactions)
	for i := 0; i < transactions; i++ {
		select {
		case ret := <-replies:
			value, ok := ret.(int)
			if !ok {
				t.Fatalf("reply %v", ret)
			}
			seen[value] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d replies delivered", i, transactions)
		}
	}
	for value := 1; value <= transactions; value++ {
		if !seen[value] {
			t.Fatalf("missing reply for commit %d: %v", value, seen)
		}
	}
	// Self-check: the scenario is only meaningful if a transaction actually
	// took the saturated fallback path.
	degraded = 0
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "nest.pipelined.async_total" && metric.Labels["result"] == "degraded" {
			degraded += metric.Value
		}
	}
	if degraded == 0 {
		t.Fatal("no transaction took the saturated fallback path: the ordering claim was not exercised")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		hooksMu.Lock()
		n := len(hooks)
		hooksMu.Unlock()
		if n == transactions || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	hooksMu.Lock()
	defer hooksMu.Unlock()
	if len(hooks) != transactions {
		t.Fatalf("hooks=%v, want %d entries", hooks, transactions)
	}
	for i, value := range hooks {
		if value != i+1 {
			t.Fatalf("AfterCommit order=%v, want [1 2 3] (commit order)", hooks)
		}
	}
}

func TestCompletionPumpOrdersEntityChainAcrossPaths(t *testing.T) {
	// The ordering primitive itself: links taken in commit order release in
	// that order, and a different entity is never blocked behind them.
	pump := newCompletionPump(1, 1)
	first := pump.link(7)
	second := pump.link(7)
	other := pump.link(8)

	// A different entity has no predecessor and proceeds immediately.
	other.await()
	other.release()

	secondRan := make(chan struct{})
	go func() {
		second.await()
		close(secondRan)
	}()
	select {
	case <-secondRan:
		t.Fatal("second completion ran before its predecessor")
	case <-time.After(20 * time.Millisecond):
	}
	first.await() // no predecessor
	first.release()
	select {
	case <-secondRan:
	case <-time.After(2 * time.Second):
		t.Fatal("second completion never unblocked")
	}
	second.release()

	// The chain entry is dropped once the tail releases.
	pump.chainMu.Lock()
	remaining := len(pump.chains)
	pump.chainMu.Unlock()
	if remaining != 0 {
		t.Fatalf("chain leaked %d entries", remaining)
	}
}

func TestCompletionPumpFullRejectsSynchronouslyAndDrainsAccepted(t *testing.T) {
	pump := newCompletionPump(1, 1)
	// Not started yet: nothing drains the queue, so the second submit must be
	// rejected synchronously — the caller holds entity locks and falls back
	// to the Phase 1 in-worker wait instead of blocking.
	ticket := &fakeCommitTicket{lsn: 1, done: make(chan struct{})}
	delivered := make(chan struct{})
	if !pump.submit(pipelinedCompletion{ticket: ticket, entityID: 1, complete: func(error) { close(delivered) }}) {
		t.Fatal("first submit should be accepted")
	}
	if pump.submit(pipelinedCompletion{ticket: ticket, entityID: 2, complete: func(error) {}}) {
		t.Fatal("full pump must reject synchronously")
	}

	// Once running, the accepted completion is delivered and stop drains it.
	pump.start()
	close(ticket.done)
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pump.stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-delivered:
	default:
		t.Fatal("accepted completion was not delivered before stop returned")
	}
	// A closed pump rejects new submissions.
	if pump.submit(pipelinedCompletion{ticket: ticket, entityID: 3, complete: func(error) {}}) {
		t.Fatal("closed pump must reject submissions")
	}
}

// The chain link is taken before the pump decides whether it can own the
// work, so on the degraded path the caller holds the release obligation while
// it waits for its own ticket. If that path unwinds — a panic while releasing
// entity locks, a ticket that never resolves — an unreleased link would block
// every later completion for that entity forever, with no timeout and no
// metric. Release is therefore idempotent and the caller defers it.
func TestCompletionOrderReleaseIsIdempotentAndUnblocksSuccessors(t *testing.T) {
	pump := newCompletionPump(1, 1)
	first := pump.link(11)
	second := pump.link(11)

	// Discharging the obligation twice (defensive defer plus the normal
	// completion path) must not panic on a double close.
	first.release()
	first.release()

	secondRan := make(chan struct{})
	go func() {
		second.await()
		close(secondRan)
	}()
	select {
	case <-secondRan:
	case <-time.After(2 * time.Second):
		t.Fatal("successor never unblocked after its predecessor released")
	}
	second.release()
	second.release()

	pump.chainMu.Lock()
	remaining := len(pump.chains)
	pump.chainMu.Unlock()
	if remaining != 0 {
		t.Fatalf("chain entries leaked: %d", remaining)
	}
}

// Concurrent releases of the same link must also be safe: the deferred
// discharge and the completion path can race in principle.
func TestCompletionOrderReleaseIsRaceSafe(t *testing.T) {
	pump := newCompletionPump(1, 1)
	order := pump.link(12)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order.release()
		}()
	}
	wg.Wait()
	select {
	case <-order.mine:
	default:
		t.Fatal("link never published as finished")
	}
}

type releaseFailureCommitter struct {
	*pipelinedTestCommitter
	outcome error
}

func (c *releaseFailureCommitter) Enqueue(ctx context.Context, record CommitRecord) (CommitTicket, error) {
	ticket, err := c.pipelinedTestCommitter.Enqueue(ctx, record)
	c.resolveAll(c.outcome)
	return ticket, err
}

// 直接走生产 NestDispatch 和 Guard；饱和用例在未启动的完成队列里占满唯一槽位，
// 确定性触发降级，不依赖生产者/消费者的运行速度。
func TestPipelinedReleasePanicStillCompletesAndReplies(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		indeterminate bool
	}{
		{"inline", false}, {"queued", false}, {"saturated", false},
		{"inline", true}, {"queued", true}, {"saturated", true},
	} {
		mode := tc.mode
		label := mode
		if tc.indeterminate {
			label += "_indeterminate"
		}
		t.Run(label, func(t *testing.T) {
			id, ent := newAsyncPilotEntity(t, 10020, 10)
			manager := entity.NewEntityManager()
			if err := manager.TryAdd(ent); err != nil {
				t.Fatal(err)
			}
			releaseErr := errors.New("release hook failed")
			defer manager.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) { panic(releaseErr) })()
			getter := newMockGetter()
			getter.Add(ent)
			committer := &releaseFailureCommitter{pipelinedTestCommitter: newPipelinedTestCommitter(false)}
			if tc.indeterminate {
				committer.outcome = ErrCommitIndeterminate
			}
			engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer))
			if mode != "inline" {
				engine.completions = newCompletionPump(1, 1)
				engine.completions.fence = engine.Fence
				if mode == "queued" {
					engine.completions.start()
				} else {
					engine.completions.queue <- pipelinedCompletion{}
				}
			}
			calls := 0
			name := NewHandlerName("release_panic_" + mode)
			engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				old := ent.dao.Value
				RecordUndo(ent.dao, 1, func() error { ent.dao.Value = old; return nil })
				ent.dao.Value = 20
				AfterCommit(func() { calls++ })
				return "committed", MarkPersist(ent.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
			msg := &Msg{Type: MsgTypeSingle, Name: name.String(), Tid: id, RetChan: make(chan any, 2)}
			NestDispatch(engine, msg)
			if mode == "queued" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := engine.completions.stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case result := <-msg.RetChan:
				if err, ok := result.(error); !ok || !errors.Is(err, releaseErr) || !errors.Is(err, ErrEntityReleaseFailed) {
					t.Errorf("reply=%v, want release failure after completion", result)
				} else if errors.Is(err, ErrCommitIndeterminate) != tc.indeterminate {
					t.Errorf("reply=%v, indeterminate=%t", result, tc.indeterminate)
				} else if errors.Is(err, ErrAfterCommitFailed) == tc.indeterminate {
					// WAL 持久后的释放失败是提交后错误，带哨兵；ticket 结果未知不是“已提交”，不带。
					t.Errorf("reply=%v: errors.Is(ErrAfterCommitFailed)=%t, want %t", result, !tc.indeterminate, tc.indeterminate)
				}
			default:
				t.Error("accepted transaction lost its reply")
			}
			if len(msg.RetChan) != 0 {
				t.Error("duplicate reply")
			}
			wantCalls := 1
			if tc.indeterminate {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Errorf("AfterCommit calls=%d, want %d", calls, wantCalls)
			}
			if committer.released.IsZero() != tc.indeterminate {
				t.Error("incorrect release notification for ticket outcome")
			}
			if errors.Is(engine.FenceError(), ErrCommitIndeterminate) != tc.indeterminate {
				t.Error("incorrect fencing for ticket outcome")
			}
			if ent.dao.Value != 20 {
				t.Error("admitted state rolled back")
			}
			if entity.CurrentGuardScope() != nil {
				t.Error("Guard scope leaked")
			}
			if !ent.GetMutex().TryLock() {
				t.Fatal("entity remained locked")
			}
			ent.GetMutex().Unlock()
			if engine.completions != nil && len(engine.completions.chains) != 0 {
				t.Error("completion ordering link leaked")
			}
		})
	}
}

func TestPipelinedInlineCompletionReportsCallbackFailure(t *testing.T) {
	for _, mode := range []string{"inline", "queued", "saturated"} {
		t.Run(mode, func(t *testing.T) {
			id, ent := newAsyncPilotEntity(t, 10021, 10)
			getter := newMockGetter()
			getter.Add(ent)
			committer := newPipelinedTestCommitter(true)
			engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer))
			if mode != "inline" {
				engine.completions = newCompletionPump(1, 1)
				if mode == "queued" {
					engine.completions.start()
				} else {
					engine.completions.queue <- pipelinedCompletion{}
				}
			}
			name := NewHandlerName("completion_callback_" + mode)
			callbacks := 0
			engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				AfterCommit(func() { panic("callback failed") })
				AfterCommit(func() { callbacks++ })
				return "committed", MarkPersist(ent.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
			msg := &Msg{Type: MsgTypeSingle, Name: name.String(), Tid: id, RetChan: make(chan any, 2)}
			NestDispatch(engine, msg)
			if mode == "queued" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := engine.completions.stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case result := <-msg.RetChan:
				if err, ok := result.(error); !ok || !errors.Is(err, ErrAfterCommitFailed) {
					t.Errorf("reply=%v, want ErrAfterCommitFailed", result)
				}
			default:
				t.Error("missing reply")
			}
			if callbacks != 1 || committer.released.IsZero() {
				t.Error("failure skipped later callbacks or release notification")
			}
		})
	}
}

func TestAsyncCompletionWaitsForEntityRelease(t *testing.T) {
	id, e := newAsyncPilotEntity(t, 9980, 10)
	manager := entity.NewEntityManager()
	if err := manager.TryAdd(e); err != nil {
		t.Fatal(err)
	}
	getter := newMockGetter()
	getter.Add(e)
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(newPipelinedTestCommitter(true)), NestOptionWithPipelinedAsyncCompletion(1, 4))
	releasing, allowRelease := make(chan struct{}), make(chan struct{})
	var allowOnce sync.Once
	allow := func() { allowOnce.Do(func() { close(allowRelease) }) }
	defer allow()
	defer manager.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) { close(releasing); <-allowRelease })()
	completed := make(chan bool, 1)
	name := NewHandlerName("completion_unlock")
	engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		AfterCommit(func() {
			free := e.GetMutex().TryLock()
			if free {
				e.GetMutex().Unlock()
			}
			completed <- free
		})
		return nil, MarkPersist(e.dao, 1)
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { allow(); _ = engine.Shutdown(context.Background()) }()
	replied := make(chan error, 1)
	go func() { _, err := engine.Request(context.Background(), name, id, nil); replied <- err }()
	select {
	case <-releasing:
	case <-time.After(time.Second):
		t.Fatal("release hook not reached")
	}
	select {
	case free := <-completed:
		t.Errorf("AfterCommit ran before release completed (lock free=%v)", free)
	case <-time.After(20 * time.Millisecond):
	}
	allow()
	select {
	case err := <-replied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing completion reply")
	}
	select {
	case free := <-completed:
		if !free {
			t.Error("AfterCommit still held entity lock")
		}
	default:
	}
}
