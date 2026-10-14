// 嵌套独立事务：已提交后外层不重排、结果未知先 fence、不得写外层已快照的实体、Remote 消息与 I/O 阶段内的限制。

package nest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// handler 内嵌套的独立事务（RunIsolatedTransaction）一旦持久提交，这条消息就按“已越过提交点”处理：外层随后以锁超时等
// 可重排错误失败时不再自动重排（重排会让已提交的独立事务再提交一次），回复带 ErrNestedTransactionCommitted。
// 独立事务写的是在自己事务里 Cast 取得的实体（外层可回滚事务已快照的实体不允许嵌套写，见本文件后面的用例）。
// 历史：RR-20260926-65。

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

// addIsolatedTarget 放入一个锁组高于 pilot 的实体，供嵌套独立事务在自己的事务里 Cast 并写入：外层没有它的快照。
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

// 外层锁超时发生在独立事务已提交之后：不重排，独立事务只提交一次。
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
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&isolatedCountingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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

// 对照：独立事务被明确拒绝（没有任何内容持久化）时，外层锁超时照旧重排，第二次执行成功，回复不带哨兵。
func TestIsolatedRejectedThenOuterLockTimeoutStillRequeues(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 36570, 1)
	target := addIsolatedTarget(t, manager, 36575)
	access := entity.NewManagerAccess(manager)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&isolatedCountingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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

// 嵌套独立事务提交结果未知（ErrCommitIndeterminate）时，框架在 RunIsolatedTransaction 返回业务之前 fence 引擎，入口与错误
// 都和消息自己的事务相同，不依赖业务是否把错误传回。
// 历史：RR-20260926-76。

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
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("rr76_iso_unknown_" + tc.name)
			var attempts atomic.Int64
			var isoErr, fencedInHandler error
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				attempts.Add(1)
				e := es[0].(*rollbackTestEntity)
				_, isoErr = RunIsolatedTransaction(context.Background(), iso, "rr76_iso", func() (any, error) {
					target := e
					if tc.castInner {
						casted, err := guardFixtureCastOne[*rollbackTestEntity](inner.GUId())
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

// 业务把结果未知传回时，消息不重排，回复同时带 ErrNestedTransactionCommitted 与 ErrCommitIndeterminate，引擎已 fence。
func TestNestedIsolatedIndeterminatePropagatedKeepsSentinels(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 43100, 1)
	access := entity.NewManagerAccess(manager)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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

// 外层可回滚事务（RollbackState / RollbackUndo）已为某实体登记回滚快照时，嵌套独立事务要持久写这个实体，会在写任何
// 持久记录之前被拒绝（ErrNestedTransactionRollbackConflict），嵌套事务自身回滚，外层快照不改——否则外层回滚会让内存
// 与已持久的结果分叉。memory 外层没有回滚快照，行为不变。
// 历史：RR-20260926-74。

type recordsCommitter struct {
	mu      sync.Mutex
	records []CommitRecord
}

func (c *recordsCommitter) Commit(_ context.Context, r CommitRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}

func (c *recordsCommitter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
}

type nestedSnapshotObservation struct {
	isoErr          error
	valueAfterIso   int
	versionAfterIso uint64
	attempts        int
}

// runOuterWithNestedWrite 注册并执行一个 handler：嵌套独立事务把 target（nil 表示声明实体）改成 100 并持久标记，随后外层失败。
func runOuterWithNestedWrite(t *testing.T, meta HandlerMeta, unique int64, castTarget bool) (p, z *rollbackTestEntity, iso *recordsCommitter, obs *nestedSnapshotObservation, reply requestResult) {
	t.Helper()
	manager := entity.NewEntityManager()
	pID, pilot := newAsyncPilotEntity(t, unique, 1)
	if err := manager.TryAdd(pilot); err != nil {
		t.Fatal(err)
	}
	zID := mustBuildCastID(t, unique+1, castPlayerCategory, castPlayerKind)
	other := &rollbackTestEntity{EntityBase: entity.NewEntityBase(zID, castPlayerCategory, false, castPlayerKind), dao: &rollbackTestDao{id: zID, Value: 1}}
	if err := manager.TryAdd(other); err != nil {
		t.Fatal(err)
	}
	access := entity.NewManagerAccess(manager)
	var outer TransactionCommitter = &recordsCommitter{}
	if meta.Durability == DurabilityPipelined {
		outer = newPipelinedTestCommitter(true)
	}
	iso = &recordsCommitter{}
	obs = &nestedSnapshotObservation{}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("rr74_nested_write")
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		obs.attempts++
		declared := es[0].(*rollbackTestEntity)
		target := declared
		_, obs.isoErr = RunIsolatedTransaction(context.Background(), iso, "rr74_iso", func() (any, error) {
			if castTarget {
				casted, err := guardFixtureCastOne[*rollbackTestEntity](zID) // 嵌套事务自己取得、自己捕获，外层没有它的快照
				if err != nil {
					return nil, err
				}
				target = casted
			}
			old := target.dao.Value
			RecordUndo(target.dao, 1, func() error { target.dao.Value = old; return nil })
			target.dao.Value = 100
			return nil, MarkPersist(target.dao, 1)
		})
		obs.valueAfterIso, obs.versionAfterIso = target.dao.Value, target.dao.Tracker.Version()
		return nil, errors.New("business failed after the isolated transaction")
	}, meta)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	reply = requestWithin(t, mgr, name, pID)
	return pilot, other, iso, obs, reply
}

// 外层可回滚事务里，嵌套独立事务写外层已快照的声明实体：被拒绝，外层快照不变。
func TestNestedIsolatedWriteToOuterSnapshotIsRefused(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}},
		{"undo_strict", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}},
		{"undo_pipelined", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, iso, obs, reply := runOuterWithNestedWrite(t, tc.meta, 42000+int64(i)*10, false)
			if n := iso.count(); n != 0 {
				t.Fatalf("isolated transaction wrote %d durable record(s) for an entity the outer %s transaction will restore (isoErr=%v, in-memory value=%d version=%d after outer rollback): it must be refused before any durable record",
					n, tc.meta.Rollback, obs.isoErr, p.dao.Value, p.dao.Tracker.Version())
			}
			if !errors.Is(obs.isoErr, ErrNestedTransactionRollbackConflict) {
				t.Fatalf("isolated transaction error=%v, want ErrNestedTransactionRollbackConflict", obs.isoErr)
			}
			if obs.valueAfterIso != 1 || obs.versionAfterIso != 0 {
				t.Fatalf("after the refused isolated transaction: value=%d version=%d, want 1/0 (it must roll itself back)", obs.valueAfterIso, obs.versionAfterIso)
			}
			if p.dao.Value != 1 || p.dao.Tracker.Version() != 0 {
				t.Fatalf("after the outer rollback: value=%d version=%d, want 1/0", p.dao.Value, p.dao.Tracker.Version())
			}
			if reply.err == nil || errors.Is(reply.err, ErrNestedTransactionCommitted) || obs.attempts != 1 {
				t.Fatalf("reply=%v attempts=%d: want the outer business error, nothing committed, one attempt", reply.err, obs.attempts)
			}
		})
	}
}

// 对照：memory 外层没有回滚快照（仓内 entity_delete.go 的删除路径即此形态），嵌套独立事务照常持久提交。
func TestNestedIsolatedWriteUnderMemoryOuterStillCommits(t *testing.T) {
	p, _, iso, obs, reply := runOuterWithNestedWrite(t, HandlerMeta{}, 42100, false)
	if iso.count() != 1 || obs.isoErr != nil {
		t.Fatalf("memory outer: isolated commits=%d err=%v, want 1/nil", iso.count(), obs.isoErr)
	}
	if p.dao.Value != 100 || p.dao.Tracker.Version() != 1 {
		t.Fatalf("memory outer: value=%d version=%d, want 100/1", p.dao.Value, p.dao.Tracker.Version())
	}
	if !errors.Is(reply.err, ErrNestedTransactionCommitted) {
		t.Fatalf("reply=%v, want ErrNestedTransactionCommitted (RR-65)", reply.err)
	}
}

// 对照：可回滚外层里，嵌套独立事务写一个外层没有快照的实体（嵌套事务内 Cast 取得）照常提交，外层回滚不碰它。
func TestNestedIsolatedWriteToUncapturedEntityCommits(t *testing.T) {
	for i, meta := range []HandlerMeta{
		{Rollback: RollbackState, Durability: DurabilityStrict},
		{Rollback: RollbackUndo, Durability: DurabilityStrict},
	} {
		p, z, iso, obs, reply := runOuterWithNestedWrite(t, meta, 42200+int64(i)*10, true)
		if iso.count() != 1 || obs.isoErr != nil {
			t.Fatalf("%s outer: isolated commits=%d err=%v, want 1/nil", meta.Rollback, iso.count(), obs.isoErr)
		}
		if z.dao.Value != 100 || z.dao.Tracker.Version() != 1 || p.dao.Value != 1 {
			t.Fatalf("%s outer: casted value=%d version=%d declared=%d, want 100/1/1", meta.Rollback, z.dao.Value, z.dao.Tracker.Version(), p.dao.Value)
		}
		if !errors.Is(reply.err, ErrNestedTransactionCommitted) {
			t.Fatalf("reply=%v, want ErrNestedTransactionCommitted (RR-65)", reply.err)
		}
	}
}

// 嵌套独立事务用导出的 AddMutation 直写原始 mutation、不经 DAO 时，按 EntityID 命中外层已快照的实体同样返回
// ErrNestedTransactionRollbackConflict，拒绝发生在写任何持久记录之前；memory 外层不变。
// 历史：RR-20260927-07。

type rawMutationForm uint8

const (
	rawMutationLegacy rawMutationForm = iota // EntityID / Database / Resource 形式
	rawMutationKey                           // DocumentKey 形式（AddMutation 同样以 Key.ID 作实体 ID）
)

func rawMutationFor(form rawMutationForm, id int64, data []byte) EntityMutation {
	if form == rawMutationKey {
		return EntityMutation{
			Key:  dataengine.DocumentKey{Database: "test", Resource: "rollback_raw", ID: id},
			Kind: dataengine.MutationPut, NextVersion: 1, Mask: 1, Schema: 1, Codec: "json", Data: data,
		}
	}
	return EntityMutation{Key: dataengine.DocumentKey{ID: id, Database: "test", Resource: "rollback_raw"}, Kind: dataengine.MutationPut,

		// runOuterWithNestedRawMutation 注册并执行一个 handler：嵌套独立事务把 target（castTarget 为假时是声明实体）改成 100，
		// 不经 MarkPersist，直接 AddMutation 一条原始 mutation；随后外层失败。
		ExpectedVersion: 0, NextVersion: 1, Data: data}
}

func runOuterWithNestedRawMutation(t *testing.T, meta HandlerMeta, unique int64, castTarget bool, form rawMutationForm) (p, z *rollbackTestEntity, iso *recordsCommitter, obs *nestedSnapshotObservation, reply requestResult) {
	t.Helper()
	manager := entity.NewEntityManager()
	pID, pilot := newAsyncPilotEntity(t, unique, 1)
	if err := manager.TryAdd(pilot); err != nil {
		t.Fatal(err)
	}
	other := addIsolatedTarget(t, manager, unique+1)
	var outer TransactionCommitter = &recordsCommitter{}
	if meta.Durability == DurabilityPipelined {
		outer = newPipelinedTestCommitter(true)
	}
	iso = &recordsCommitter{}
	obs = &nestedSnapshotObservation{}
	mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(manager)), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("rr0927_07_nested_raw_mutation")
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		obs.attempts++
		target := es[0].(*rollbackTestEntity)
		_, obs.isoErr = RunIsolatedTransaction(context.Background(), iso, "rr0927_07_iso", func() (any, error) {
			if castTarget {
				casted, err := guardFixtureCastOne[*rollbackTestEntity](other.GUId()) // 嵌套事务自己取得，外层没有它的快照
				if err != nil {
					return nil, err
				}
				target = casted
			}
			old := target.dao.Value
			RecordUndo(target.dao, 1, func() error { target.dao.Value = old; return nil })
			target.dao.Value = 100
			return nil, CurrentRollbackTx().AddMutation(rawMutationFor(form, target.GUId(), target.dao.Marshal()))
		})
		obs.valueAfterIso = target.dao.Value
		return nil, errors.New("business failed after the isolated transaction")
	}, meta)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	reply = requestWithin(t, mgr, name, pID)
	return pilot, other, iso, obs, reply
}

func TestNestedIsolatedRawMutationToOuterSnapshotIsRefused(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
		form rawMutationForm
	}{
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, rawMutationLegacy},
		{"undo_strict", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, rawMutationLegacy},
		{"undo_pipelined", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}, rawMutationLegacy},
		{"state_strict_key_form", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, rawMutationKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, iso, obs, reply := runOuterWithNestedRawMutation(t, tc.meta, 44000+int64(i)*10, false, tc.form)
			if n := iso.count(); n != 0 {
				t.Fatalf("isolated transaction wrote %d durable record(s) via a raw AddMutation for an entity the outer %s transaction will restore (isoErr=%v, in-memory value=%d after outer rollback): it must be refused before any durable record",
					n, tc.meta.Rollback, obs.isoErr, p.dao.Value)
			}
			if !errors.Is(obs.isoErr, ErrNestedTransactionRollbackConflict) {
				t.Fatalf("isolated transaction error=%v, want ErrNestedTransactionRollbackConflict", obs.isoErr)
			}
			if obs.valueAfterIso != 1 || p.dao.Value != 1 {
				t.Fatalf("value after the refused isolated transaction=%d, after the outer rollback=%d; want 1/1", obs.valueAfterIso, p.dao.Value)
			}
			if reply.err == nil || errors.Is(reply.err, ErrNestedTransactionCommitted) || obs.attempts != 1 {
				t.Fatalf("reply=%v attempts=%d: want the outer business error, nothing committed, one attempt", reply.err, obs.attempts)
			}
		})
	}
}

// 对照：memory 外层没有回滚快照，原始 mutation 照常提交。
func TestNestedIsolatedRawMutationUnderMemoryOuterStillCommits(t *testing.T) {
	p, _, iso, obs, reply := runOuterWithNestedRawMutation(t, HandlerMeta{}, 44100, false, rawMutationLegacy)
	if iso.count() != 1 || obs.isoErr != nil || p.dao.Value != 100 {
		t.Fatalf("memory outer: isolated commits=%d err=%v value=%d, want 1/nil/100", iso.count(), obs.isoErr, p.dao.Value)
	}
	if !errors.Is(reply.err, ErrNestedTransactionCommitted) {
		t.Fatalf("reply=%v, want ErrNestedTransactionCommitted (RR-65)", reply.err)
	}
}

// 保持性：可回滚外层里，原始 mutation 写外层没有快照的实体（嵌套事务内 Cast 取得）照常提交，外层回滚不碰它。
func TestNestedIsolatedRawMutationToUncapturedEntityCommits(t *testing.T) {
	for i, meta := range []HandlerMeta{
		{Rollback: RollbackState, Durability: DurabilityStrict},
		{Rollback: RollbackUndo, Durability: DurabilityStrict},
	} {
		p, z, iso, obs, reply := runOuterWithNestedRawMutation(t, meta, 44200+int64(i)*10, true, rawMutationLegacy)
		if iso.count() != 1 || obs.isoErr != nil {
			t.Fatalf("%s outer: isolated commits=%d err=%v, want 1/nil", meta.Rollback, iso.count(), obs.isoErr)
		}
		if z.dao.Value != 100 || p.dao.Value != 1 {
			t.Fatalf("%s outer: casted value=%d declared=%d, want 100/1", meta.Rollback, z.dao.Value, p.dao.Value)
		}
		if !errors.Is(reply.err, ErrNestedTransactionCommitted) {
			t.Fatalf("reply=%v, want ErrNestedTransactionCommitted (RR-65)", reply.err)
		}
	}
}

// 带 Remote 批次的消息里，handler 内嵌套的独立事务直接返回 ErrNestedTransactionInRemoteMessage：不执行、不 finalize、
// 不改 remoteFinalized / remoteCommitted，Remote 批次只随消息自己的事务 Commit 或 Abort。
// 历史：RR-20260926-75。
func TestNestedIsolatedInRemoteMessageIsRefused(t *testing.T) {
	for i, tc := range []struct {
		name       string
		meta       HandlerMeta
		outerFails bool
	}{
		{"memory_outer_fails", HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, true},
		{"memory_outer_succeeds", HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, false},
		{"state_strict_outer_fails", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, 44000+int64(i)*10, 10)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, 44001+int64(i)*10, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			batch := &recordingRemoteBatch{}
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			outer, iso := &recordsCommitter{}, &recordsCommitter{}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName(fmt.Sprintf("rr75_outer_%d", i))
			var isoErr error
			var isoRuns atomic.Int64
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				_, isoErr = RunIsolatedTransaction(context.Background(), iso, "rr75_iso_in_remote", func() (any, error) {
					isoRuns.Add(1)
					old := e.dao.Value
					RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
					e.dao.Value = 77
					return nil, MarkPersist(e.dao, 1)
				})
				if tc.outerFails {
					return nil, errors.New("outer business failed after the isolated call")
				}
				return "ok", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			reply := stagedWait(t, ch)
			replyErr, _ := reply.(error)
			finalized := batch.finalizedHandlers()
			if slices.Contains(finalized, "rr75_iso_in_remote") || iso.count() != 0 || isoRuns.Load() != 0 {
				t.Fatalf("nested isolated transaction went through the message's Remote batch: FinalizeLocked handlers=%v isolatedRuns=%d isolatedCommits=%d isoErr=%v reply=%v",
					finalized, isoRuns.Load(), iso.count(), isoErr, reply)
			}
			if tc.outerFails {
				if c, a := batch.commits.Load(), batch.aborts.Load(); c != 0 || a != 1 {
					t.Fatalf("outer failed: batch Commit=%d Abort=%d, want 0/1 (reply=%v)", c, a, reply)
				}
				if replyErr == nil || errors.Is(replyErr, ErrAfterCommitFailed) || errors.Is(replyErr, ErrNestedTransactionCommitted) {
					t.Fatalf("outer failed: reply=%v must not claim a commit", reply)
				}
			} else {
				if c, a := batch.commits.Load(), batch.aborts.Load(); c != 1 || a != 0 || !slices.Equal(finalized, []string{name.String()}) {
					t.Fatalf("outer succeeded: batch Commit=%d Abort=%d FinalizeLocked=%v, want 1/0/[%s] (reply=%v)", c, a, finalized, name, reply)
				}
				if reply != "ok" {
					t.Fatalf("outer succeeded: reply=%v, want ok", reply)
				}
			}
			if !errors.Is(isoErr, ErrNestedTransactionInRemoteMessage) {
				t.Fatalf("RunIsolatedTransaction in a Remote message returned %v, want ErrNestedTransactionInRemoteMessage", isoErr)
			}
			if e.dao.Value != 10 {
				t.Fatalf("value=%d, want 10: the refused isolated call must not run", e.dao.Value)
			}
		})
	}
}

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

// isolatedInPrepare 记录I/O 阶段准备里那一次 RunIsolatedTransaction 的观察结果（只在第一次读取时调用）。
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
	o.inMessage, o.inFast = currentNestDispatchMsg() != nil, fctx.InShortWorker()
	_, o.err = RunIsolatedTransaction(context.Background(), iso, "b19_iso_in_prepare", func() (any, error) {
		o.callRan = true
		return nil, CurrentRollbackTx().AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: target, Database: "test", Resource: "b19_prepare"}, Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: []byte(`{}`)})
	})
}

func TestIsolatedInIOPrepareOfLocalMessageDoesNotClaimIt(t *testing.T) {
	id := mustBuildCastID(t, 48600, entity.EntityCategory(1), nestLocalKind)
	obs := &isolatedInPrepare{}
	iso := &recordsCommitter{}
	getter := &prepareHookGetter{mockGetter: newMockGetter(), hook: func() { obs.run(iso, id) }}
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	outer := &recordsCommitter{}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{1, 8}))
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
	_, err := mgr.Request(ctx, name, id, nil, SendOptionIO())

	if !obs.inMessage || obs.inFast {
		t.Fatalf("premise: the isolated call must run in the message's I/O preparation stage (in message=%v, business worker=%v)", obs.inMessage, obs.inFast)
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

func TestIsolatedInIOPrepareOfRemoteMessageIsRefused(t *testing.T) {
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
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(remote), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
		t.Fatalf("premise: the isolated call must run in the message's I/O preparation stage (in message=%v, business worker=%v)", obs.inMessage, obs.inFast)
	}
	if !errors.Is(obs.err, ErrNestedTransactionInRemoteMessage) || obs.callRan || iso.count() != 0 {
		t.Fatalf("Remote message: isolated err=%v ran=%v commits=%d, want ErrNestedTransactionInRemoteMessage with the body not executed", obs.err, obs.callRan, iso.count())
	}
	if reply != "ok" || batchCommits.Load() != 1 || batch.aborted.Load() {
		t.Fatalf("reply=%v batch commits=%d aborted=%v: the refused isolated call must not touch the message or its batch", reply, batchCommits.Load(), batch.aborted.Load())
	}
}

type releaseNotifyingCommitter struct {
	recordsCommitter
	onReleased func()
}

func (c *releaseNotifyingCommitter) TransactionReleased(TransactionID) {
	if c.onReleased != nil {
		c.onReleased()
	}
}

func TestIsolatedInClosingPhaseOfRemoteMessageIsRefused(t *testing.T) {
	for i, tc := range []struct {
		name        string
		meta        HandlerMeta
		outerFails  bool
		afterUnlock bool // true：在解锁后回调（TransactionReleased）里调用；否则在 Guard post-release 回调里
	}{
		{"memory_outer_fails", HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, true, false},
		{"state_strict_outer_fails", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, true, false},
		{"memory_outer_succeeds", HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, false, false},
		{"state_strict_outer_succeeds_after_unlock", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, 48400+int64(i)*10, 10)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, 48401+int64(i)*10, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			batch := &recordingRemoteBatch{}
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			outer, iso := &releaseNotifyingCommitter{}, &recordsCommitter{}
			var isoErr error
			var isoCalled, isoRuns atomic.Int64
			isolated := func() {
				isoCalled.Add(1)
				_, isoErr = RunIsolatedTransaction(context.Background(), iso, "rr84_iso_in_closing", func() (any, error) {
					isoRuns.Add(1)
					old := e.dao.Value
					RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
					e.dao.Value = 55
					return nil, MarkPersist(e.dao, 1)
				})
			}
			if tc.afterUnlock {
				outer.onReleased = isolated
			}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName(fmt.Sprintf("rr84_remote_closing_%d", i))
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if !tc.afterUnlock {
					scope := entity.CurrentGuardScope()
					if scope == nil || scope.Guard() == nil {
						return nil, errors.New("test premise: no guard scope")
					}
					scope.Guard().AppendPostRelease(isolated)
				}
				if tc.outerFails {
					return nil, errors.New("outer business failed")
				}
				return "ok", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			reply := stagedWait(t, ch)
			replyErr, _ := reply.(error)
			finalized := batch.finalizedHandlers()
			if isoCalled.Load() != 1 {
				t.Fatalf("test premise: the closing-phase callback ran %d times, want 1 (reply=%v)", isoCalled.Load(), reply)
			}
			if slices.Contains(finalized, "rr84_iso_in_closing") || iso.count() != 0 || isoRuns.Load() != 0 {
				t.Fatalf("isolated transaction in the closing phase went through the message's Remote batch: FinalizeLocked=%v isolatedRuns=%d isolatedCommits=%d isoErr=%v batch Commit=%d Abort=%d reply=%v",
					finalized, isoRuns.Load(), iso.count(), isoErr, batch.commits.Load(), batch.aborts.Load(), reply)
			}
			if !errors.Is(isoErr, ErrNestedTransactionInRemoteMessage) {
				t.Fatalf("RunIsolatedTransaction in the closing phase of a Remote message returned %v, want ErrNestedTransactionInRemoteMessage", isoErr)
			}
			if tc.outerFails {
				if c, a := batch.commits.Load(), batch.aborts.Load(); c != 0 || a != 1 {
					t.Fatalf("outer failed: batch Commit=%d Abort=%d, want 0/1 (reply=%v)", c, a, reply)
				}
				if replyErr == nil || errors.Is(replyErr, ErrAfterCommitFailed) || errors.Is(replyErr, ErrNestedTransactionCommitted) {
					t.Fatalf("outer failed: reply=%v must not claim a commit", reply)
				}
			} else {
				if c, a := batch.commits.Load(), batch.aborts.Load(); c != 1 || a != 0 || !slices.Equal(finalized, []string{name.String()}) {
					t.Fatalf("outer succeeded: batch Commit=%d Abort=%d FinalizeLocked=%v, want 1/0/[%s] (reply=%v)", c, a, finalized, name, reply)
				}
				if reply != "ok" {
					t.Fatalf("outer succeeded: reply=%v, want ok", reply)
				}
			}
			if e.dao.Value != 10 {
				t.Fatalf("value=%d, want 10: the refused isolated call must not run", e.dao.Value)
			}
		})
	}
}

func TestIsolatedInClosingPhaseOfLocalMessageDoesNotClaimIt(t *testing.T) {
	for i, tc := range []struct {
		name          string
		meta          HandlerMeta
		outerErr      error // 第一次执行时外层返回的错误；nil 表示成功
		indeterminate bool  // 回调里的独立事务提交结果未知
	}{
		{"undo_strict_outer_fails", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, errors.New("outer failed"), false},
		{"memory_outer_fails", HandlerMeta{}, errors.New("outer failed"), false},
		{"undo_strict_outer_lock_timeout", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, fmt.Errorf("%w: outer contended", ErrLockTimeout), false},
		{"undo_strict_outer_ok_hook_indeterminate", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, nil, true},
		{"memory_outer_ok_hook_indeterminate", HandlerMeta{}, nil, true},
		{"undo_strict_outer_ok_hook_commits", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pID, p := newAsyncPilotEntity(t, 48500+int64(i)*10, 1)
			if err := manager.TryAdd(p); err != nil {
				t.Fatal(err)
			}
			access := entity.NewManagerAccess(manager)
			outer := &recordsCommitter{}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			hookCommits := &recordsCommitter{}
			var hookCommitter TransactionCommitter = hookCommits
			if tc.indeterminate {
				hookCommitter = &indeterminateCommitter{}
			}
			var hookErr error
			var attempts atomic.Int64
			name := NewHandlerName("rr84_local_closing_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				n := attempts.Add(1)
				declared := es[0].(*rollbackTestEntity)
				entity.CurrentGuardScope().Guard().AppendPostRelease(func() {
					_, hookErr = RunIsolatedTransaction(context.Background(), hookCommitter, "rr84_hook_iso", func() (any, error) {
						old := declared.dao.Value
						RecordUndo(declared.dao, 1, func() error { declared.dao.Value = old; return nil })
						declared.dao.Value = 40
						return nil, MarkPersist(declared.dao, 1)
					})
				})
				if tc.outerErr != nil && n == 1 {
					return nil, tc.outerErr
				}
				return "ok", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			r := requestWithin(t, mgr, name, pID)
			if attempts.Load() != 1 {
				t.Fatalf("attempts=%d reply=%v/%v hookErr=%v: a message whose closing-phase isolated transaction committed must not be requeued", attempts.Load(), r.ret, r.err, hookErr)
			}
			if errors.Is(r.err, ErrAfterCommitFailed) {
				t.Fatalf("reply=%v claims the message's own transaction committed (outer commits=%d, hookErr=%v): the closing-phase isolated transaction must not claim the message", r.err, outer.count(), hookErr)
			}
			switch {
			case tc.indeterminate:
				fenced := mgr.FenceError()
				if !errors.Is(hookErr, ErrCommitIndeterminate) || !errors.Is(fenced, ErrNestFenced) || !errors.Is(fenced, ErrCommitIndeterminate) {
					t.Fatalf("closing-phase isolated transaction outcome unknown: hookErr=%v FenceError=%v reply=%v/%v, want the engine fenced like any nested isolated transaction (RR-76)", hookErr, fenced, r.ret, r.err)
				}
				if r.err != nil || r.ret != "ok" {
					t.Fatalf("reply=%v/%v, want ok: the message's own transaction committed before the closing phase", r.ret, r.err)
				}
				second := requestWithin(t, mgr, name, pID)
				if !errors.Is(second.err, ErrNestFenced) || attempts.Load() != 1 {
					t.Fatalf("second request reply=%v/%v attempts=%d: a fenced engine must refuse new work", second.ret, second.err, attempts.Load())
				}
			case tc.outerErr != nil:
				if hookErr != nil || hookCommits.count() != 1 || p.dao.Value != 40 {
					t.Fatalf("premise: hookErr=%v hookCommits=%d value=%d, want the closing-phase isolated transaction committed once", hookErr, hookCommits.count(), p.dao.Value)
				}
				if !errors.Is(r.err, ErrNestedTransactionCommitted) || !errors.Is(r.err, tc.outerErr) {
					t.Fatalf("reply=%v, want ErrNestedTransactionCommitted wrapping the outer error (RR-65)", r.err)
				}
				if outer.count() != 0 {
					t.Fatalf("outer commits=%d, want 0: the outer transaction failed", outer.count())
				}
			default:
				if hookErr != nil || hookCommits.count() != 1 || r.err != nil || r.ret != "ok" {
					t.Fatalf("hookErr=%v hookCommits=%d reply=%v/%v, want ok and one closing-phase commit", hookErr, hookCommits.count(), r.ret, r.err)
				}
				if err := mgr.FenceError(); err != nil {
					t.Fatalf("FenceError=%v, want nil", err)
				}
			}
		})
	}
}
