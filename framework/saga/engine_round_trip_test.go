package saga

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// countingClaimStore 数 ClaimOutbox 交出命令的次数：派发后的命令若不经领取直接发布，这个数保持 0。
type countingClaimStore struct {
	*memoryStore
	claimedOutbox atomic.Int64
}

func (s *countingClaimStore) ClaimOutbox(ctx context.Context, q ClaimRequest) ([]OutboxRecord, error) {
	items, err := s.memoryStore.ClaimOutbox(ctx, q)
	s.claimedOutbox.Add(int64(len(items)))
	return items, err
}

// dispatchedOn 在 store 上建一个不运行循环的引擎，启动一笔 saga 并手工派发第一步，返回派发后的记录。
func dispatchedOn(t *testing.T, store Store) (*Engine, Record) {
	t.Helper()
	engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "dispatch-1"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: "c", Now: started.NextRunAt, LeaseDuration: time.Second, Limit: 1})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if err := engine.processClaimed(ctx, claimed[0], started.NextRunAt); err != nil {
		t.Fatal(err)
	}
	waiting, err := store.Get(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	return engine, waiting
}

// TestCompleteDispatchesNextStepInSameApply 是 v2 第 2c 段（完成即派发）的验收用例：接收成功的同一次 Apply 已派发下一步
// （Waiting、attempt 1、outbox 有新命令、版本只加 1），不经过 ClaimDue。当前引擎接收结果后记录停在 Pending、等协调循环
// 领取再派发，所以这条在第 2c 段实施之前是红的。
func TestCompleteDispatchesNextStepInSameApply(t *testing.T) {
	t.Skip("第 2c 段（完成即派发）实施时去掉；当前引擎上为红，见 DESIGN-2026-10-26-redis-authority-v2.md 实施结果")
	store := newMemoryStore()
	engine, waiting := dispatchedOn(t, store)
	after, err := engine.Complete(context.Background(), Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusWaiting || after.Step != 1 || after.Attempt != 1 || after.Version != waiting.Version+1 {
		t.Fatalf("after the first step's success: status=%v step=%d attempt=%d version=%d (before %d); want Waiting on step 1, attempt 1, version +1",
			after.Status, after.Step, after.Attempt, after.Version, waiting.Version)
	}
	if _, queued := store.outbox[after.CommandID]; !queued {
		t.Fatalf("outbox has no command %q for the next step", after.CommandID)
	}
}

// TestDispatchPublishesWithoutClaim 是第 2c 段（派发后直接发布）的验收用例：派发的命令不经 ClaimOutbox 交给发布器。
// 当前发布器只经领取拿命令，在第 2c 段实施之前是红的。
func TestDispatchPublishesWithoutClaim(t *testing.T) {
	t.Skip("第 2c 段（派发后直接发布）实施时去掉；当前引擎上为红，见 DESIGN-2026-10-26-redis-authority-v2.md 实施结果")
	store := &countingClaimStore{memoryStore: newMemoryStore()}
	published := make(chan Command, 4)
	options := DefaultOptions()
	options.CoordinatorWorkers, options.PublisherWorkers = 1, 1
	options.LeaseDuration, options.StoreTimeout, options.PublishTimeout = time.Second, 100*time.Millisecond, 100*time.Millisecond
	engine, err := NewEngine(store, PublishFunc(func(_ context.Context, c Command) error { published <- c; return nil }), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = engine.Run(ctx) }()
	if _, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "publish-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("no command published")
	}
	if n := store.claimedOutbox.Load(); n != 0 {
		t.Fatalf("the dispatched command went through ClaimOutbox %d time(s); want it handed to the publisher directly", n)
	}
}

// TestAckAfterCloseIsNotAConflict：完成先于发布确认到达时（结果关闭了操作、删掉了它的命令），发布器之后的确认返回 nil，
// 不计冲突。
func TestAckAfterCloseIsNotAConflict(t *testing.T) {
	ctx := context.Background()
	store := newTestRedisStore(t)
	engine, waiting := dispatchedOn(t, store)
	conflicts := engine.Stats().Conflicts
	claimed, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "publisher", Now: waiting.UpdatedAt, LeaseDuration: time.Second, Limit: 1})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim outbox = %+v, %v", claimed, err)
	}
	if _, err := engine.Complete(ctx, Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.AckOutbox(ctx, claimed[0].Command.ID, claimed[0].Lease); err != nil {
		t.Fatalf("ack after the completion closed the operation = %v (ErrConflict=%v); want nil", err, errors.Is(err, ErrConflict))
	}
	if grown := engine.Stats().Conflicts - conflicts; grown != 0 {
		t.Fatalf("conflicts grew by %d", grown)
	}
}

// TestCompleteDoesNotFoldRetryBackoff：可重试失败之后的退避不与下一次派发合并——记录停在 Pending、NextRunAt 在将来，
// 没有新命令，到期后由协调循环派发。
func TestCompleteDoesNotFoldRetryBackoff(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	engine, waiting := dispatchedOn(t, store)
	after, err := engine.Complete(ctx, Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Retryable: true, Error: "try again"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusPending || after.Step != 0 || after.Attempt != 1 || !after.NextRunAt.After(after.UpdatedAt) || after.CommandID != "" {
		t.Fatalf("after a retryable failure: %+v; want Pending on the same step with a future NextRunAt", after)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.outbox) != 1 {
		t.Fatalf("outbox has %d commands; the backoff must not queue the next attempt", len(store.outbox))
	}
}
