package nest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-65：消息执行期间，handler 内嵌套的独立事务（RunIsolatedTransaction）一旦持久提交，
// 这条消息就按“已越过提交点”处理（与 RR-20260926-49 同一判据）：外层随后以锁超时等可重排错误失败时不再
// 自动重排（重排会让已提交的独立事务再提交一次），回复带 ErrNestedTransactionCommitted 供调用方判别。
// RR-20260926-74 之后，嵌套独立事务不能再写外层可回滚事务已快照的实体（声明实体就是），所以这里的独立事务改为写一个
// 在自己事务里 Cast 取得的实体；RR-65 的承诺本身（已提交不重排、回复带哨兵）不变。

type isolatedCountingCommitter struct {
	calls   atomic.Int64
	rejectN int64 // 前 rejectN 次调用明确拒绝
}

func (c *isolatedCountingCommitter) Commit(context.Context, CommitRecord) error {
	if n := c.calls.Add(1); n <= c.rejectN {
		return fmt.Errorf("isolated commit %d refused", n)
	}
	return nil
}

type isolatedRun struct {
	attempts, isoRuns atomic.Int64
	value             *rollbackTestEntity
	iso               *isolatedCountingCommitter
}

// addIsolatedTarget 放入一个锁组高于 pilot 的实体，供嵌套独立事务在自己的事务里 Cast 并写入：外层没有它的快照（RR-20260926-74）。
func addIsolatedTarget(t *testing.T, manager *entity.EntityManager, unique int64) *rollbackTestEntity {
	t.Helper()
	id := mustBuildCastID(t, unique, castPlayerCategory, castPlayerKind)
	e := &rollbackTestEntity{EntityBase: entity.NewEntityBase(id, castPlayerCategory, false, castPlayerKind), dao: &rollbackTestDao{id: id, Value: 1}}
	if err := manager.TryAdd(e); err != nil {
		t.Fatal(err)
	}
	return e
}

// registerIsolatedThenFail 注册一个 handler：先在 RunIsolatedTransaction 里 Cast run.value 并修改、持久提交，第一次执行随后返回 outerErr。
func registerIsolatedThenFail(t *testing.T, mgr *NestMgr, name HandlerName, meta HandlerMeta, run *isolatedRun, outerErr error) {
	t.Helper()
	targetID := run.value.GUId()
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		n := run.attempts.Add(1)
		if _, err := RunIsolatedTransaction(context.Background(), run.iso, "rr65_iso", func() (any, error) {
			run.isoRuns.Add(1)
			e, err := guardFixtureCastOne[*rollbackTestEntity](targetID)
			if err != nil {
				return nil, err
			}
			old := e.dao.Value
			if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
				return nil, errors.New("missing isolated transaction")
			}
			e.dao.Value++
			return nil, MarkPersist(e.dao, 1)
		}); err != nil && n > 1 {
			return nil, err
		}
		if n == 1 {
			return nil, outerErr
		}
		return "ok", nil
	}, meta)
}

func requestWithin(t *testing.T, mgr *NestMgr, name HandlerName, id int64) requestResult {
	t.Helper()
	out := make(chan requestResult, 1)
	sendRequest(mgr, "r", name, id, out)
	select {
	case r := <-out:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not reply within 10s", name)
		return requestResult{}
	}
}

// REPRO-2026-09-26-06 §1 P-C：修前 attempts=2 isolatedRuns=2 isolatedCommits=2——外层锁超时重排后独立事务再提交一次。
func TestIsolatedCommitThenOuterLockTimeoutIsNotRequeued(t *testing.T) {
	for i, tc := range []struct {
		name      string
		meta      HandlerMeta
		pipelined bool
	}{
		{"undo_strict", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, false},
		{"memory", HandlerMeta{}, false},
		{"undo_pipelined", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			ids := addPilots(t, manager, 36500+int64(i)*10, 1)
			target := addIsolatedTarget(t, manager, 36505+int64(i)*10)
			access := entity.NewManagerAccess(manager)
			var outer TransactionCommitter = &isolatedCountingCommitter{}
			if tc.pipelined {
				outer = newPipelinedTestCommitter(true)
			}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			run := &isolatedRun{iso: &isolatedCountingCommitter{}, value: target}
			name := NewHandlerName("rr65_iso_then_lock_timeout_" + tc.name)
			registerIsolatedThenFail(t, mgr, name, tc.meta, run, fmt.Errorf("%w: simulated lock conflict after the isolated commit", ErrLockTimeout))
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			r := requestWithin(t, mgr, name, ids[0])
			if a, runs, commits := run.attempts.Load(), run.isoRuns.Load(), run.iso.calls.Load(); a != 1 || runs != 1 || commits != 1 {
				t.Fatalf("one request: attempts=%d isolatedRuns=%d isolatedCommits=%d value=%d reply=%v/%v; a message whose nested isolated transaction committed must not be requeued",
					a, runs, commits, run.value.dao.Value, r.ret, r.err)
			}
			if run.value.dao.Value != 2 { // pilot 初值 1，独立事务只加一次
				t.Fatalf("value=%d, want 2", run.value.dao.Value)
			}
			if !errors.Is(r.err, ErrNestedTransactionCommitted) || !errors.Is(r.err, ErrLockTimeout) {
				t.Fatalf("reply=%v: want ErrNestedTransactionCommitted with the outer cause kept", r.err)
			}
			if errors.Is(r.err, ErrAfterCommitFailed) {
				t.Fatalf("reply=%v: the message's own transaction did not commit, it must not claim ErrAfterCommitFailed", r.err)
			}
		})
	}
}

// 独立事务已提交后外层以普通业务错误失败：不涉及重排，但回复同样带 ErrNestedTransactionCommitted，
// 调用方能区分“什么都没提交”与“部分已提交”。
func TestIsolatedCommitThenOuterFailureCarriesSentinel(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 36560, 1)
	target := addIsolatedTarget(t, manager, 36565)
	access := entity.NewManagerAccess(manager)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&isolatedCountingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	run := &isolatedRun{iso: &isolatedCountingCommitter{}, value: target}
	boom := errors.New("business failed after the isolated commit")
	name := NewHandlerName("rr65_iso_then_boom")
	registerIsolatedThenFail(t, mgr, name, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, run, boom)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	r := requestWithin(t, mgr, name, ids[0])
	if !errors.Is(r.err, ErrNestedTransactionCommitted) || !errors.Is(r.err, boom) || run.attempts.Load() != 1 {
		t.Fatalf("reply=%v attempts=%d: want ErrNestedTransactionCommitted wrapping the business error, one attempt", r.err, run.attempts.Load())
	}
}

// 保持性：独立事务被明确拒绝（没有任何内容持久化）时，外层锁超时照旧重排，第二次执行成功，回复不带哨兵。
func TestIsolatedRejectedThenOuterLockTimeoutStillRequeues(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 36570, 1)
	target := addIsolatedTarget(t, manager, 36575)
	access := entity.NewManagerAccess(manager)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&isolatedCountingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	run := &isolatedRun{iso: &isolatedCountingCommitter{rejectN: 1}, value: target}
	name := NewHandlerName("rr65_iso_rejected_then_lock_timeout")
	registerIsolatedThenFail(t, mgr, name, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, run, fmt.Errorf("%w: simulated", ErrLockTimeout))
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	r := requestWithin(t, mgr, name, ids[0])
	if r.err != nil || r.ret != "ok" || run.attempts.Load() != 2 || run.iso.calls.Load() != 2 {
		t.Fatalf("reply=%v/%v attempts=%d isolatedCommits=%d: an uncommitted nested transaction must not block the requeue", r.ret, r.err, run.attempts.Load(), run.iso.calls.Load())
	}
	if run.value.dao.Value != 2 { // 第一次独立事务被拒绝并回滚，第二次提交
		t.Fatalf("value=%d, want 2", run.value.dao.Value)
	}
}
