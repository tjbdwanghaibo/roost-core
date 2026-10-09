package nest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-74：外层可回滚事务（RollbackState / RollbackUndo）已为某实体登记回滚快照时，handler 内嵌套的独立事务
// （RunIsolatedTransaction）要持久写这个实体，会在写任何持久记录之前被拒绝（ErrNestedTransactionRollbackConflict），
// 嵌套事务自身回滚，外层快照不改。旧行为（REPRO-2026-09-26-07 §1）：独立事务持久提交 next=1 value=100 后外层失败回滚，
// RollbackState 把内存值与 tracker 版本都退回（value=1、version=0），RollbackUndo 值保留而版本退回（value=100、version=0），
// 内存与已持久的结果分叉。memory 外层（仓内 entity_delete 的删除路径）没有回滚快照，行为不变。

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
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("rr74_nested_write")
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		obs.attempts++
		declared := es[0].(*rollbackTestEntity)
		target := declared
		_, obs.isoErr = RunIsolatedTransaction(context.Background(), iso, "rr74_iso", func() (any, error) {
			if castTarget {
				casted, err := CastOne[*rollbackTestEntity](zID) // 嵌套事务自己取得、自己捕获，外层没有它的快照
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

// REPRO-2026-09-26-07 §1 探针 2 改写：外层可回滚事务里，嵌套独立事务写外层已快照的声明实体。
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

// 保持性：memory 外层没有回滚快照（仓内 entity_delete.go 的删除路径即此形态），嵌套独立事务照常持久提交。
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

// 保持性：可回滚外层里，嵌套独立事务写一个外层没有快照的实体（嵌套事务内 Cast 取得）照常提交，外层回滚不碰它。
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
