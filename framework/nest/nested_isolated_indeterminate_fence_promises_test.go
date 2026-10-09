package nest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-76：handler 内嵌套的独立事务提交结果未知（ErrCommitIndeterminate）时，框架在 RunIsolatedTransaction 返回业务之前
// fence 引擎，入口与错误都和消息自己的事务相同（invokeHandlerTransaction 里的 mgr.Fence），不依赖业务是否把错误传回。
// 旧行为（REPRO-2026-09-26-07 §1 探针 3a）：业务吞掉结果未知后外层照常提交、回复 ok、FenceError=<nil>，后续请求照常受理。

type indeterminateCommitter struct{ calls atomic.Int64 }

func (c *indeterminateCommitter) Commit(context.Context, CommitRecord) error {
	c.calls.Add(1)
	return fmt.Errorf("%w: simulated WAL outcome unknown", ErrCommitIndeterminate)
}

func TestNestedIsolatedIndeterminateFencesBeforeReturning(t *testing.T) {
	for i, tc := range []struct {
		name      string
		meta      HandlerMeta
		castInner bool // 外层可回滚时独立事务写自己 Cast 的实体（RR-74 不允许写外层已快照的声明实体）
	}{
		{"undo_strict", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, true},
		{"memory", HandlerMeta{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pID, p := newAsyncPilotEntity(t, 43000+int64(i)*10, 1)
			if err := manager.TryAdd(p); err != nil {
				t.Fatal(err)
			}
			inner := addIsolatedTarget(t, manager, 43001+int64(i)*10)
			access := entity.NewManagerAccess(manager)
			iso := &indeterminateCommitter{}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			name := NewHandlerName("rr76_iso_unknown_" + tc.name)
			var attempts atomic.Int64
			var isoErr, fencedInHandler error
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				attempts.Add(1)
				e := es[0].(*rollbackTestEntity)
				_, isoErr = RunIsolatedTransaction(context.Background(), iso, "rr76_iso", func() (any, error) {
					target := e
					if tc.castInner {
						casted, err := CastOne[*rollbackTestEntity](inner.GUId())
						if err != nil {
							return nil, err
						}
						target = casted
					}
					old := target.dao.Value
					RecordUndo(target.dao, 1, func() error { target.dao.Value = old; return nil })
					target.dao.Value = 50
					return nil, MarkPersist(target.dao, 1)
				})
				fencedInHandler = mgr.FenceError() // RunIsolatedTransaction 返回业务时引擎是否已 fence
				// 业务吞掉结果未知，继续在声明实体上做外层修改并成功返回。
				old := e.dao.Value
				RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
				e.dao.Value++
				if tc.meta.Rollback == RollbackNone {
					return "ok", nil // memory 快路径没有事务可标记持久
				}
				return "ok", MarkPersist(e.dao, 1)
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			first := requestWithin(t, mgr, name, pID)
			if !errors.Is(isoErr, ErrCommitIndeterminate) || iso.calls.Load() != 1 {
				t.Fatalf("premise: isolated commit calls=%d err=%v, want one indeterminate commit", iso.calls.Load(), isoErr)
			}
			if fencedInHandler == nil || !errors.Is(fencedInHandler, ErrNestFenced) || !errors.Is(fencedInHandler, ErrCommitIndeterminate) {
				t.Fatalf("when RunIsolatedTransaction returned an indeterminate outcome the engine was not fenced: FenceError=%v (first reply=%v/%v)", fencedInHandler, first.ret, first.err)
			}
			second := requestWithin(t, mgr, name, pID)
			if !errors.Is(second.err, ErrNestFenced) || attempts.Load() != 1 {
				t.Fatalf("second request reply=%v/%v attempts=%d: a fenced engine must refuse new work", second.ret, second.err, attempts.Load())
			}
		})
	}
}

// 保持性：业务把结果未知传回时，消息按 RR-65 不重排，回复同时带 ErrNestedTransactionCommitted 与 ErrCommitIndeterminate，引擎已 fence。
func TestNestedIsolatedIndeterminatePropagatedKeepsSentinels(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 43100, 1)
	access := entity.NewManagerAccess(manager)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("rr76_iso_unknown_propagated")
	var attempts atomic.Int64
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		attempts.Add(1)
		e := es[0].(*rollbackTestEntity)
		return RunIsolatedTransaction(context.Background(), &indeterminateCommitter{}, "rr76_iso", func() (any, error) {
			e.dao.Value = 50
			return nil, MarkPersist(e.dao, 1)
		})
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	r := requestWithin(t, mgr, name, ids[0])
	if !errors.Is(r.err, ErrNestedTransactionCommitted) || !errors.Is(r.err, ErrCommitIndeterminate) || attempts.Load() != 1 {
		t.Fatalf("reply=%v attempts=%d: want ErrNestedTransactionCommitted + ErrCommitIndeterminate, one attempt", r.err, attempts.Load())
	}
	if fenced := mgr.FenceError(); !errors.Is(fenced, ErrNestFenced) || !errors.Is(fenced, ErrCommitIndeterminate) {
		t.Fatalf("FenceError=%v, want ErrNestFenced wrapping the indeterminate outcome", fenced)
	}
}
