package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260927-07（OPEN-ITEMS C06）：RR-74 的拒绝只看 MarkPersist 登记的 DAO 参与方。嵌套独立事务用导出的 AddMutation 直写原始
// mutation、不经 DAO 时，旧实现照常提交：外层随后失败回滚，把快照恢复到内存，持久是 100、内存是 1（audit4 探针 B：
// isoErr=<nil> isoCommits=1 in-memory value after outer rollback=1）。现在原始 mutation 按 EntityID 命中外层已快照的实体时
// 同样返回 ErrNestedTransactionRollbackConflict，拒绝发生在写任何持久记录之前；memory 外层不变。

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
	return EntityMutation{EntityID: id, Database: "test", Resource: "rollback_raw", Version: 1, Data: data}
}

// runOuterWithNestedRawMutation 注册并执行一个 handler：嵌套独立事务把 target（castTarget 为假时是声明实体）改成 100，
// 不经 MarkPersist，直接 AddMutation 一条原始 mutation；随后外层失败。
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
	mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(manager)), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := NewHandlerName("rr0927_07_nested_raw_mutation")
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		obs.attempts++
		target := es[0].(*rollbackTestEntity)
		_, obs.isoErr = RunIsolatedTransaction(context.Background(), iso, "rr0927_07_iso", func() (any, error) {
			if castTarget {
				casted, err := CastOne[*rollbackTestEntity](other.GUId()) // 嵌套事务自己取得，外层没有它的快照
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

// 保持性（RR-74）：memory 外层没有回滚快照，原始 mutation 照常提交。
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
