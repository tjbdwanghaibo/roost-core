// Pipelined 异步完成：持久等待不占业务 worker，同实体完成顺序、完成池饱和降级、释放失败仍回复且只回复一次。

package nest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
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
		NestOptionWithPipelinedAsyncCompletion(0, 0, "test_async_frees_worker"),
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
		NestOptionWithPipelinedAsyncCompletion(0, 0, "test_async_order_first", "test_async_order_second"),
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
		NestOptionWithPipelinedAsyncCompletion(0, 0, "test_async_indeterminate"),
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
		NestOptionWithPipelinedAsyncCompletion(0, 0, "test_async_shutdown_drain"),
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
		NestOptionWithPipelinedAsyncCompletion(1, 1, "test_async_saturated_order"),
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

// stop 的超时只结束本次等待：重试必须继续等同一批已接受的完成排空，不能因为“已经发起过”就返回 nil。
func TestCompletionPumpStopRetryWaitsForDrain(t *testing.T) {
	pump := newCompletionPump(1, 1)
	pump.start()
	ticket := &fakeCommitTicket{lsn: 1, done: make(chan struct{})}
	delivered := make(chan struct{})
	if !pump.submit(pipelinedCompletion{ticket: ticket, entityID: 1, complete: func(error) { close(delivered) }}) {
		t.Fatal("submit should be accepted")
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pump.stop(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("first stop with a cancelled ctx = %v, want context.Canceled", err)
	}
	retry, cancelRetry := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelRetry()
	if err := pump.stop(retry); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry while the accepted completion is still pending = %v, want context.DeadlineExceeded", err)
	}

	close(ticket.done)
	if err := pump.stop(context.Background()); err != nil {
		t.Fatalf("stop after the ticket resolved = %v", err)
	}
	select {
	case <-delivered:
	default:
		t.Fatal("stop returned before the accepted completion was delivered")
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
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(newPipelinedTestCommitter(true)), NestOptionWithPipelinedAsyncCompletion(1, 4, "completion_unlock"))
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

// 异步完成按 handler 生效（维护者 2026-10-29 决定）：引擎的异步完成名单只点名别的 handler 时，strict、async 与没被点名的
// pipelined handler 在“有完成泵 / 没有完成泵”两种引擎上的观察完全一致——strict / async 走 Commit（不 Enqueue），pipelined
// 走 Enqueue 后在派发 worker 上等票据；AfterCommit 都在派发 goroutine 上、回复之前执行，执行时实体锁的状态也相同。
func TestAsyncCompletionLeavesNonPipelinedHandlersUnchanged(t *testing.T) {
	type observation struct {
		commits, enqueued int
		lockFreeInHook    bool
		hookBeforeReply   bool
		hookOnDispatch    bool
		reply             any
	}
	run := func(t *testing.T, durability DurabilityPolicy, withPump bool) observation {
		t.Helper()
		id, e := newAsyncPilotEntity(t, 9990, 10)
		getter := newMockGetter()
		getter.Add(e)
		committer := newPipelinedTestCommitter(true)
		options := []NestOption{NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer)}
		if withPump {
			// 名单点名的是另一个 handler：引擎有完成泵，但被测 handler 不在名单里。
			options = append(options, NestOptionWithPipelinedAsyncCompletion(1, 4, "someone_else"))
		}
		engine := NewEngine(options...)
		var hookRan, lockFree, onDispatch atomic.Bool
		name := NewHandlerName("non_async_" + durability.String())
		engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			AfterCommit(func() {
				free := e.GetMutex().TryLock()
				if free {
					e.GetMutex().Unlock()
				}
				lockFree.Store(free)
				onDispatch.Store(currentNestDispatchMsg() != nil)
				hookRan.Store(true)
			})
			old := e.dao.Value
			RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
			e.dao.Value++
			return e.dao.Value, MarkPersist(e.dao, 1)
		}, HandlerMeta{Rollback: RollbackState, Durability: durability})
		if err := engine.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = engine.Shutdown(context.Background()) }()
		reply, err := engine.Request(context.Background(), name, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		hookBeforeReply := hookRan.Load()
		committer.mu.Lock()
		defer committer.mu.Unlock()
		return observation{commits: committer.commits, enqueued: len(committer.tickets), lockFreeInHook: lockFree.Load(), hookBeforeReply: hookBeforeReply, hookOnDispatch: onDispatch.Load(), reply: reply}
	}
	for _, durability := range []DurabilityPolicy{DurabilityStrict, DurabilityAsync, DurabilityPipelined} {
		off, on := run(t, durability, false), run(t, durability, true)
		if on != off {
			t.Errorf("%s handler outside the async set: engine with completion pump = %+v, without = %+v; want identical", durability, on, off)
		}
		if !on.hookBeforeReply || !on.hookOnDispatch || on.reply != 11 {
			t.Errorf("%s handler outside the async set = %+v; want AfterCommit on the dispatch goroutine before the reply", durability, on)
		}
		wantCommits, wantEnqueued := 1, 0
		if durability == DurabilityPipelined {
			wantCommits, wantEnqueued = 0, 1
		}
		if on.commits != wantCommits || on.enqueued != wantEnqueued {
			t.Errorf("%s handler outside the async set = %+v; want %d Commit, %d Enqueue", durability, on, wantCommits, wantEnqueued)
		}
	}
}

// asyncSetEngine 是名单 = {asyncName} 的引擎：asyncName 与 phase1Name 都是 pipelined 自增 handler，AfterCommit 按
// “标签 + 自增后的值”记录，并记下回调是否在派发 goroutine 上；gates 里登记的标签在回调里先报 entered、再等放行。
type asyncSetEngine struct {
	engine     *NestMgr
	committer  *pipelinedTestCommitter
	mu         sync.Mutex
	hooks      []string
	onDispatch map[string]bool
	gates      map[string]*hookGate
	entered    chan string
}

func newAsyncSetEngine(t *testing.T, workers int, asyncName, phase1Name HandlerName, es ...*rollbackTestEntity) *asyncSetEngine {
	t.Helper()
	return newAsyncSetEngineWith(t, workers, asyncName, phase1Name, nil, es...)
}

// newAsyncSetEngineWith 同 newAsyncSetEngine，extra 在 Start 之前再注册别的 handler。
func newAsyncSetEngineWith(t *testing.T, workers int, asyncName, phase1Name HandlerName, extra func(*NestMgr), es ...*rollbackTestEntity) *asyncSetEngine {
	t.Helper()
	getter := newMockGetter()
	for _, e := range es {
		getter.Add(e)
	}
	f := &asyncSetEngine{committer: newPipelinedTestCommitter(false), onDispatch: map[string]bool{}, gates: map[string]*hookGate{}, entered: make(chan string, 8)}
	f.engine = NewEngine(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(f.committer),
		NestOptionWithPipelinedAsyncCompletion(2, 64, asyncName.String()),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: workers, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
	)
	register := func(name HandlerName, tag string) {
		f.engine.MustRegisterHandlerWithMeta(name, func(targets []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			e := targets[0].(*rollbackTestEntity)
			old := e.dao.Value
			RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
			e.dao.Value++
			label := tag + strconv.Itoa(e.dao.Value)
			AfterCommit(func() {
				onDispatch := currentNestDispatchMsg() != nil
				if gate := f.gates[label]; gate != nil {
					f.entered <- label
					<-gate.ch
				}
				f.mu.Lock()
				f.hooks = append(f.hooks, label)
				f.onDispatch[label] = onDispatch
				f.mu.Unlock()
			})
			return e.dao.Value, MarkPersist(e.dao, 1)
		}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
	}
	register(asyncName, "A")
	register(phase1Name, "B")
	if extra != nil {
		extra(f.engine)
	}
	if err := f.engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.engine.Shutdown(ctx)
	})
	// 后注册先执行：用例中途失败时先放行全部回调，Shutdown 才能排空。
	t.Cleanup(func() {
		for _, gate := range f.gates {
			gate.open()
		}
		f.committer.resolveAll(nil)
	})
	return f
}

// hookGate 卡住一个 AfterCommit，open 幂等。
type hookGate struct {
	ch   chan struct{}
	once sync.Once
}

func (g *hookGate) open() { g.once.Do(func() { close(g.ch) }) }

// gate 在请求之前登记：标签为 label 的回调先报 entered，再等返回的 hookGate 放行。
func (f *asyncSetEngine) gate(label string) *hookGate {
	g := &hookGate{ch: make(chan struct{})}
	f.gates[label] = g
	return g
}

// request 异步发一个请求，回复（值或错误）进返回的 channel。
func (f *asyncSetEngine) request(name HandlerName, id int64) chan any {
	done := make(chan any, 1)
	go func() {
		ret, err := f.engine.Request(context.Background(), name, id, nil)
		if err != nil {
			done <- err
			return
		}
		done <- ret
	}()
	return done
}

func (f *asyncSetEngine) recorded() ([]string, map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hooks), maps.Clone(f.onDispatch)
}

func awaitSignal[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

func expectSilence[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s: got %v", what, v)
	case <-time.After(100 * time.Millisecond):
	}
}

// 名单 = {A} 的引擎上：A 的事务交给完成泵（派发 worker 在票据落定前就空出来，AfterCommit 在完成池上），B 同样声明
// pipelined 但不在名单里，保持第一阶段（派发 worker 停在票据上，AfterCommit 在派发 goroutine 上、回复之前执行）。
func TestAsyncCompletionIsPerHandler(t *testing.T) {
	asyncName, phase1Name, bystander := NewHandlerName("per_handler_async"), NewHandlerName("per_handler_phase1"), NewHandlerName("per_handler_bystander")
	idA, entA := newAsyncPilotEntity(t, 9970, 10)
	idB, entB := newAsyncPilotEntity(t, 9971, 10)
	idC, entC := newAsyncPilotEntity(t, 9972, 0)
	f := newAsyncSetEngineWith(t, 1, asyncName, phase1Name, func(engine *NestMgr) {
		engine.MustRegisterHandlerWithMeta(bystander, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			return "bystander", nil
		}, HandlerMeta{})
	}, entA, entB, entC)

	t.Run("named handler uses the completion pump", func(t *testing.T) {
		replyA := f.request(asyncName, idA)
		awaitSignal(t, f.committer.enqueued, "A enqueued")
		if got := awaitSignal(t, f.request(bystander, idC), "bystander while A's ticket is pending"); got != "bystander" {
			t.Fatalf("bystander = %v", got)
		}
		expectSilence(t, replyA, "A replied before its ticket resolved")
		f.committer.resolveAll(nil)
		if got := awaitSignal(t, replyA, "A reply"); got != 11 {
			t.Fatalf("A reply = %v, want 11", got)
		}
		waitHooks(t, f, 1)
		if _, onDispatch := f.recorded(); onDispatch["A11"] {
			t.Fatal("A's AfterCommit ran on the dispatch goroutine; want the completion pool")
		}
	})
	t.Run("pipelined handler outside the set stays on phase 1", func(t *testing.T) {
		replyB := f.request(phase1Name, idB)
		awaitSignal(t, f.committer.enqueued, "B enqueued")
		other := f.request(bystander, idC)
		expectSilence(t, other, "bystander ran while B's ticket was pending: the only worker was released (async completion for a handler outside the set)")
		f.committer.resolveAll(nil)
		if got := awaitSignal(t, replyB, "B reply"); got != 11 {
			t.Fatalf("B reply = %v, want 11", got)
		}
		hooks, onDispatch := f.recorded()
		if !slices.Contains(hooks, "B11") {
			t.Fatalf("B's AfterCommit had not run when B replied: hooks=%v", hooks)
		}
		if !onDispatch["B11"] {
			t.Fatal("B's AfterCommit ran off the dispatch goroutine; want phase 1 (in the dispatch worker, before the reply)")
		}
		awaitSignal(t, other, "bystander after B")
	})
}

func waitHooks(t *testing.T, f *asyncSetEngine, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hooks, _ := f.recorded()
		if len(hooks) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hooks = %v, want %d", hooks, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// 同一实体先后被异步完成的 A 与第一阶段的 B 修改：AfterCommit 与回复都按 LSN 序。
//   - A1 → B2 → A3：A1 的 AfterCommit 卡住时，B2 的票据已落定也不能先执行 AfterCommit、先回复（B2 在完成链上排在 A1 后面）；
//     A3 要等 B2 的作业（含回复）结束才开始（同 ID 准入），所以 A3 的一切都在 B2 之后。
//   - B1 → A2：A2 要等 B1 的作业结束才开始执行，B1 的 AfterCommit 卡住时 A2 连 Enqueue 都没有发生。
func TestAsyncAndPhase1CompletionsKeepSameEntityLSNOrder(t *testing.T) {
	asyncName, phase1Name := NewHandlerName("lsn_order_async"), NewHandlerName("lsn_order_phase1")
	t.Run("async then phase1 then async", func(t *testing.T) {
		id, e := newAsyncPilotEntity(t, 9975, 0)
		f := newAsyncSetEngine(t, 4, asyncName, phase1Name, e)
		gate := f.gate("A1")
		replyA1 := f.request(asyncName, id)
		awaitSignal(t, f.committer.enqueued, "A1 enqueued")
		replyB2 := f.request(phase1Name, id)
		awaitSignal(t, f.committer.enqueued, "B2 enqueued")
		replyA3 := f.request(asyncName, id)
		expectSilence(t, f.committer.enqueued, "A3 enqueued while B2's job was still running")
		f.committer.resolveAll(nil) // LSN 1、2 都已持久
		if got := awaitSignal(t, f.entered, "A1 AfterCommit"); got != "A1" {
			t.Fatalf("first AfterCommit = %s, want A1", got)
		}
		expectSilence(t, replyB2, "B2 replied while A1 (LSN 1) had not completed")
		if hooks, _ := f.recorded(); len(hooks) != 0 {
			t.Fatalf("AfterCommit ran ahead of A1: %v", hooks)
		}
		gate.open()
		if got := awaitSignal(t, replyA1, "A1 reply"); got != 1 {
			t.Fatalf("A1 reply = %v, want 1", got)
		}
		if got := awaitSignal(t, replyB2, "B2 reply"); got != 2 {
			t.Fatalf("B2 reply = %v, want 2", got)
		}
		awaitSignal(t, f.committer.enqueued, "A3 enqueued after B2")
		f.committer.resolveAll(nil)
		if got := awaitSignal(t, replyA3, "A3 reply"); got != 3 {
			t.Fatalf("A3 reply = %v, want 3", got)
		}
		waitHooks(t, f, 3)
		hooks, onDispatch := f.recorded()
		if !slices.Equal(hooks, []string{"A1", "B2", "A3"}) {
			t.Fatalf("AfterCommit order = %v, want [A1 B2 A3]", hooks)
		}
		if onDispatch["A1"] || !onDispatch["B2"] || onDispatch["A3"] {
			t.Fatalf("AfterCommit goroutines = %v; want A on the completion pool, B on the dispatch goroutine", onDispatch)
		}
	})
	t.Run("phase1 then async", func(t *testing.T) {
		id, e := newAsyncPilotEntity(t, 9976, 0)
		f := newAsyncSetEngine(t, 4, asyncName, phase1Name, e)
		gate := f.gate("B1")
		replyB1 := f.request(phase1Name, id)
		awaitSignal(t, f.committer.enqueued, "B1 enqueued")
		replyA2 := f.request(asyncName, id)
		expectSilence(t, f.committer.enqueued, "A2 enqueued while B1's job was still running")
		f.committer.resolveAll(nil)
		if got := awaitSignal(t, f.entered, "B1 AfterCommit"); got != "B1" {
			t.Fatalf("first AfterCommit = %s, want B1", got)
		}
		expectSilence(t, f.committer.enqueued, "A2 enqueued while B1's AfterCommit was running")
		expectSilence(t, replyA2, "A2 replied before B1 completed")
		gate.open()
		if got := awaitSignal(t, replyB1, "B1 reply"); got != 1 {
			t.Fatalf("B1 reply = %v, want 1", got)
		}
		awaitSignal(t, f.committer.enqueued, "A2 enqueued after B1")
		f.committer.resolveAll(nil)
		if got := awaitSignal(t, replyA2, "A2 reply"); got != 2 {
			t.Fatalf("A2 reply = %v, want 2", got)
		}
		waitHooks(t, f, 2)
		if hooks, _ := f.recorded(); !slices.Equal(hooks, []string{"B1", "A2"}) {
			t.Fatalf("AfterCommit order = %v, want [B1 A2]", hooks)
		}
	})
}
