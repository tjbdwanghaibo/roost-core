package nest_test

// OPEN-ITEMS B03 / RR-20260927-06：handler 内嵌套独立事务的 committer 已成功（真实 nestwal 追加并落盘），但 DAO 的 AcceptMutation
// 返回错误，acceptPersistence 把它报成 ErrCommitIndeterminate，RR-76 据此 fence 引擎。这种来源下 WAL 并没有进入 terminal，
// 外层 handler 继续执行，它自己的事务如果照常交给 committer 会被真实 WAL 接受——fence 之后引擎仍在写。
// 放在 nest 目录的外部测试包里：nestwal 依赖 nest，包内测试不能 import 它。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

const (
	// 与包内测试不重叠的 kind；类别按锁序：声明实体在前，嵌套事务 Cast 的实体在后。
	fenceOuterPilotKind  entity.EntityKind     = 206
	fenceOuterInnerKind  entity.EntityKind     = 207
	fenceOuterPilotGroup entity.EntityCategory = entity.EntityCategoryRemote + 1
	fenceOuterInnerGroup entity.EntityCategory = entity.EntityCategoryRemote + 2
)

var errFenceOuterAcceptFailed = errors.New("simulated AcceptMutation failure after the WAL append")

type fenceOuterDao struct {
	id         int64
	Tracker    dataengine.Tracker
	Value      int
	failAccept bool
}

func (d *fenceOuterDao) Id() int64                         { return d.id }
func (d *fenceOuterDao) SetId(id int64)                    { d.id = id }
func (d *fenceOuterDao) DbName() string                    { return "test" }
func (d *fenceOuterDao) CollName() string                  { return "fence_outer" }
func (d *fenceOuterDao) Dirty() entity.IDirty              { return &d.Tracker }
func (d *fenceOuterDao) CleanDirty()                       { d.Tracker.SelfClean() }
func (d *fenceOuterDao) DirtyTracker() *dataengine.Tracker { return &d.Tracker }

func (d *fenceOuterDao) marshal() []byte {
	raw, _ := json.Marshal(struct {
		ID    int64 `json:"id"`
		Value int   `json:"value"`
	}{d.id, d.Value})
	return raw
}

func (d *fenceOuterDao) CaptureRollbackState() ([]byte, error) { return d.marshal(), nil }
func (d *fenceOuterDao) RestoreRollbackState(raw []byte) error {
	var doc struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	d.Value = doc.Value
	return nil
}

func (d *fenceOuterDao) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error) {
	version := d.Tracker.Version()
	return dataengine.Mutation{
		Key:  dataengine.DocumentKey{Database: "test", Resource: d.CollName(), ID: d.id},
		Kind: dataengine.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "json", Data: d.marshal(),
	}, nil
}

func (d *fenceOuterDao) AcceptMutation(mutation dataengine.Mutation) error {
	if d.failAccept {
		return errFenceOuterAcceptFailed
	}
	return d.Tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

type fenceOuterEntity struct {
	*entity.EntityBase
	dao *fenceOuterDao
}

func (e *fenceOuterEntity) Base() *entity.EntityBase { return e.EntityBase }
func (e *fenceOuterEntity) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.dao)
	}
}

func addFenceOuterEntity(t *testing.T, manager *entity.EntityManager, unique int64, kind entity.EntityKind, category entity.EntityCategory) *fenceOuterEntity {
	t.Helper()
	entity.MustRegisterEntityKindCategory(kind, category)
	id, err := entity.BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	e := &fenceOuterEntity{EntityBase: entity.NewEntityBase(id, category, false, kind), dao: &fenceOuterDao{id: id, Value: 1}}
	if err := manager.TryAdd(e); err != nil {
		t.Fatal(err)
	}
	return e
}

type discardApplier struct{}

func (discardApplier) ApplyMutations(context.Context, nest.TransactionID, []nest.EntityMutation) error {
	return nil
}

func openFenceOuterCommitter(t *testing.T) (*nestwal.WAL, *nestwal.Committer) {
	t.Helper()
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	committer, err := nestwal.NewCommitter(wal, discardApplier{}, nil, nestwal.CommitterOptions{CloseWAL: true})
	if err != nil {
		_ = wal.Close(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = committer.Close(ctx)
	})
	return wal, committer
}

// fence 之后，外层自己的提交在交给 committer 之前返回 ErrNestFenced：真实 WAL 只有嵌套事务那一条记录，外层修改回滚。
func TestOuterCommitAfterNestedAcceptFailureIsRefusedBeforeWAL(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta nest.HandlerMeta
	}{
		{"undo_strict", nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict}},
		{"state_strict", nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict}},
		{"undo_pipelined", nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityPipelined}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pilot := addFenceOuterEntity(t, manager, 52000+int64(i)*10, fenceOuterPilotKind, fenceOuterPilotGroup)
			inner := addFenceOuterEntity(t, manager, 52001+int64(i)*10, fenceOuterInnerKind, fenceOuterInnerGroup)
			inner.dao.failAccept = true
			wal, committer := openFenceOuterCommitter(t)
			mgr := nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(manager)), nest.NestOptionWithTransactionCommitter(committer), nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			name := nest.NewHandlerName("b03_fence_then_outer_commit_" + tc.name)
			var isoErr, fencedInHandler error
			var appendedAfterNested uint64
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
				declared := es[0].(*fenceOuterEntity)
				_, isoErr = nest.RunIsolatedTransaction(context.Background(), committer, "b03_iso", func() (any, error) {
					casted, err := nest.CastOne[*fenceOuterEntity](inner.GUId())
					if err != nil {
						return nil, err
					}
					old := casted.dao.Value
					nest.RecordUndo(casted.dao, 1, func() error { casted.dao.Value = old; return nil })
					casted.dao.Value = 9
					return nil, nest.MarkPersist(casted.dao, 1)
				}) // 业务吞掉结果未知
				fencedInHandler = mgr.FenceError()
				appendedAfterNested = wal.Stats().Admitted
				old := declared.dao.Value
				nest.RecordUndo(declared.dao, 1, func() error { declared.dao.Value = old; return nil })
				declared.dao.Value = 7
				return "ok", nest.MarkPersist(declared.dao, 1)
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ret, err := mgr.Request(ctx, name, pilot.GUId(), nil)

			if !errors.Is(isoErr, nest.ErrCommitIndeterminate) || !errors.Is(isoErr, errFenceOuterAcceptFailed) || appendedAfterNested != 1 {
				t.Fatalf("premise: nested isolated err=%v WAL admitted=%d, want the WAL append to succeed and AcceptMutation to fail (indeterminate)", isoErr, appendedAfterNested)
			}
			if !errors.Is(fencedInHandler, nest.ErrNestFenced) {
				t.Fatalf("premise (RR-76): FenceError=%v inside the handler, want ErrNestFenced", fencedInHandler)
			}
			if terminal := wal.Stats().TerminalError; terminal != "" {
				t.Fatalf("premise: the WAL went terminal (%s); this item is about the non-terminal acceptPersistence source", terminal)
			}
			if admitted := wal.Stats().Admitted; admitted != 1 {
				t.Fatalf("after the engine was fenced the outer transaction was still handed to the WAL: admitted=%d (want 1, the nested record only) reply=%v/%v", admitted, ret, err)
			}
			// RR-20260927-32：交给 committer 之前的拒绝同时带 ErrCommitRejected（判别表仍先命中 ErrNestedTransactionCommitted 那一行，RR-20260928-03 起为第 5 行）。
			if !errors.Is(err, nest.ErrNestFenced) || !errors.Is(err, nest.ErrNestedTransactionCommitted) || !errors.Is(err, nest.ErrCommitRejected) || errors.Is(err, nest.ErrCommitIndeterminate) {
				t.Fatalf("reply=%v/%v: want ErrNestFenced + ErrCommitRejected (refused before the committer, so not indeterminate) + ErrNestedTransactionCommitted", ret, err)
			}
			if pilot.dao.Value != 1 || pilot.dao.Tracker.Version() != 0 {
				t.Fatalf("outer changes were not rolled back: value=%d version=%d, want 1/0", pilot.dao.Value, pilot.dao.Tracker.Version())
			}
		})
	}
}
