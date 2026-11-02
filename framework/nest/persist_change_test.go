package nest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

type fakeMutationParticipant struct {
	tracker    dataengine.Tracker
	prepared   []PersistChange
	accepted   []dataengine.Mutation
	prepareErr error
	acceptErr  error
}

type dataEngineTrackedRollbackDAO struct {
	tracker dataengine.Tracker
}

func (d *dataEngineTrackedRollbackDAO) Id() int64                         { return 7 }
func (d *dataEngineTrackedRollbackDAO) SetId(int64)                       {}
func (d *dataEngineTrackedRollbackDAO) DbName() string                    { return "game" }
func (d *dataEngineTrackedRollbackDAO) CollName() string                  { return "hero" }
func (d *dataEngineTrackedRollbackDAO) Dirty() entity.IDirty              { return &d.tracker }
func (d *dataEngineTrackedRollbackDAO) CleanDirty()                       { d.tracker.SelfClean() }
func (d *dataEngineTrackedRollbackDAO) DirtyTracker() *dataengine.Tracker { return &d.tracker }

func (p *fakeMutationParticipant) PrepareMutation(change PersistChange) (dataengine.Mutation, error) {
	p.prepared = append(p.prepared, change.Clone())
	if p.prepareErr != nil {
		return dataengine.Mutation{}, p.prepareErr
	}
	version := p.tracker.Version()
	mutation := dataengine.Mutation{
		Key:             dataengine.DocumentKey{Database: "game", Resource: "hero", ID: 7},
		Kind:            dataengine.MutationPut,
		ExpectedVersion: version,
		NextVersion:     version + 1,
		Mask:            change.Mask,
		Data:            []byte{1},
	}
	if change.Delete {
		mutation.Kind = dataengine.MutationDelete
		mutation.Data = nil
	}
	return mutation, nil
}

func (p *fakeMutationParticipant) AcceptMutation(mutation dataengine.Mutation) error {
	p.accepted = append(p.accepted, dataengine.CloneMutation(mutation))
	if p.acceptErr != nil {
		return p.acceptErr
	}
	return p.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

func TestMarkPersistRequiresActiveTransaction(t *testing.T) {
	if err := MarkPersist(&fakeMutationParticipant{}, 1); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("err = %v, want ErrTransactionClosed", err)
	}
}

func TestPersistChangeCoalescesOneParticipant(t *testing.T) {
	tx := NewRollbackTx(RollbackState)
	p := &fakeMutationParticipant{}
	if err := tx.MarkPersistSet(p, 1, "level", 2); err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkPersistSet(p, 2, "exp", 9); err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkPersistUnset(p, 4, "profile.title"); err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkPersistSet(p, 8, "profile.title", "rookie"); err != nil {
		t.Fatal(err)
	}

	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Mutations) != 1 || len(p.prepared) != 1 {
		t.Fatalf("mutations=%d prepare calls=%d", len(record.Mutations), len(p.prepared))
	}
	change := p.prepared[0]
	if change.Mask != 15 || change.Set["level"] != 2 || change.Set["exp"] != 9 || change.Set["profile.title"] != "rookie" {
		t.Fatalf("coalesced change = %+v", change)
	}
	if len(change.Unset) != 0 {
		t.Fatalf("set must remove matching unset: %+v", change.Unset)
	}
	if record.Mutations[0].Mask != 15 {
		t.Fatalf("mutation mask=%d, want 15", record.Mutations[0].Mask)
	}
}

func TestMarkPersistFullDropsCoveredSubpaths(t *testing.T) {
	tx := NewRollbackTx(RollbackState)
	p := &fakeMutationParticipant{}
	if err := tx.MarkPersistSet(p, 1, "profile.level", 3); err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkPersistUnset(p, 2, "profile.title"); err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkPersistFull(p, 4, "profile"); err != nil {
		t.Fatal(err)
	}
	change := tx.participantChanges[p]
	if _, ok := change.FullFields["profile"]; !ok || len(change.Set) != 0 || len(change.Unset) != 0 {
		t.Fatalf("full field did not cover child paths: %+v", change)
	}
}

func TestAddReceiptDeduplicatesAndRejectsDigestConflict(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	receipt := dataengine.Receipt{Namespace: "saga-step", ID: "step-1", Digest: []byte{1}}
	if err := tx.AddReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	if len(tx.receipts) != 1 {
		t.Fatalf("receipts=%d, want 1", len(tx.receipts))
	}
	receipt.Digest = []byte{2}
	if err := tx.AddReceipt(receipt); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("err=%v, want ErrReceiptConflict", err)
	}
}

func TestSetReceiptPayloadPreservesBoundIdentity(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	if err := tx.AddReceipt(dataengine.Receipt{Namespace: "saga-step", ID: "command-1", Digest: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetReceiptPayload("saga-step", "command-1", []byte("completion")); err != nil {
		t.Fatal(err)
	}
	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Receipts) != 1 || string(record.Receipts[0].Payload) != "completion" || string(record.Receipts[0].Digest) != "\x01" {
		t.Fatalf("receipts=%+v", record.Receipts)
	}
	if err := tx.SetReceiptPayload("saga-step", "missing", nil); err == nil {
		t.Fatal("unbound receipt payload was accepted")
	}
}

func TestPipelinedEnqueueAcceptsVersionBeforeReturning(t *testing.T) {
	p := &fakeMutationParticipant{}
	tx := NewRollbackTx(RollbackUndo)
	tx.durability = DurabilityPipelined
	if err := tx.MarkPersist(p, 1); err != nil {
		t.Fatal(err)
	}
	committer := newPipelinedTestCommitter(false)
	ticket, err := tx.pipelinedEnqueue(nil, committer)
	if err != nil {
		t.Fatal(err)
	}
	if ticket == nil || p.tracker.Version() != 1 || len(p.accepted) != 1 {
		t.Fatalf("ticket=%v version=%d accepted=%d", ticket, p.tracker.Version(), len(p.accepted))
	}

	successor := NewRollbackTx(RollbackUndo)
	if err := successor.MarkPersist(p, 2); err != nil {
		t.Fatal(err)
	}
	record, err := successor.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if got := record.Mutations[0].ExpectedVersion; got != 1 {
		t.Fatalf("successor expected version=%d, want 1", got)
	}
}

func TestStrictCommitAcceptsPreparedMutation(t *testing.T) {
	p := &fakeMutationParticipant{}
	tx := NewRollbackTx(RollbackUndo)
	tx.durability = DurabilityStrict
	if err := tx.MarkPersist(p, 1); err != nil {
		t.Fatal(err)
	}
	committer := &recordingCommitter{}
	if err := tx.durableCommit(nil, committer); err != nil {
		t.Fatal(err)
	}
	if p.tracker.Version() != 1 || len(p.accepted) != 1 {
		t.Fatalf("version=%d accepted=%d", p.tracker.Version(), len(p.accepted))
	}
}

func TestAcceptFailureAfterAdmissionIsIndeterminate(t *testing.T) {
	p := &fakeMutationParticipant{acceptErr: errors.New("tracker fenced")}
	tx := NewRollbackTx(RollbackUndo)
	tx.durability = DurabilityPipelined
	if err := tx.MarkPersist(p, 1); err != nil {
		t.Fatal(err)
	}
	ticket, err := tx.pipelinedEnqueue(nil, newPipelinedTestCommitter(false))
	if ticket == nil || !errors.Is(err, ErrCommitIndeterminate) {
		t.Fatalf("ticket=%v err=%v", ticket, err)
	}
}

func TestRollbackDiscardsPersistChangeWithoutPreparingParticipant(t *testing.T) {
	p := &fakeMutationParticipant{}
	tx := NewRollbackTx(RollbackUndo)
	if err := tx.MarkPersistSet(p, 1, "level", 2); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if len(p.prepared) != 0 || tx.participantChanges != nil || tx.participantOrder != nil {
		t.Fatalf("prepared=%d changes=%v order=%v", len(p.prepared), tx.participantChanges, tx.participantOrder)
	}
}

func TestRemotePersistChangeIsTransactionLocalAndExcludedFromOrdinaryMutations(t *testing.T) {
	p := &fakeMutationParticipant{}
	tx := NewRollbackTx(RollbackUndo)
	if err := tx.MarkPersistSet(p, 0b01, "profile.level", 7); err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkPersistUnset(p, 0b10, "profile.title"); err != nil {
		t.Fatal(err)
	}

	change, ok := tx.RemotePersistChangeFor(p)
	if !ok || change.Mask != 0b11 || change.Delete {
		t.Fatalf("remote change = %+v, ok=%v", change, ok)
	}
	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Mutations) != 0 {
		t.Fatalf("remote participant leaked into ordinary mutations: %+v", record.Mutations)
	}
	if len(p.prepared) != 0 || len(p.accepted) != 0 {
		t.Fatalf("remote participant prepared=%d accepted=%d", len(p.prepared), len(p.accepted))
	}
}

func TestPipelinedAcceptFailureDoesNotRollbackAndFencesNest(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 390, 1, nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, 1, false, nestLocalKind),
		dao:        dao,
	})
	p := &fakeMutationParticipant{acceptErr: errors.New("version tracker failed")}
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}),
	)
	defer StopNest()
	MustRegisterHandlerWithMeta(NewHandlerName("test_pipelined_accept_failure"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		current := es[0].(*rollbackTestEntity).dao
		old := current.Value
		if !RecordUndo(current, 1, func() error { current.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		current.Value = 20
		if err := MarkPersist(p, 1); err != nil {
			return nil, err
		}
		return "indeterminate", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})

	ret, err := Nest.Request(context.Background(), NewHandlerName("test_pipelined_accept_failure"), id, nil)
	if ret != nil || !errors.Is(err, ErrCommitIndeterminate) {
		t.Fatalf("ret=%v err=%v", ret, err)
	}
	if dao.Value != 20 {
		t.Fatalf("value=%d: admitted mutation was rolled back", dao.Value)
	}
	if fence := Nest.FenceError(); !errors.Is(fence, ErrNestFenced) || !errors.Is(fence, ErrCommitIndeterminate) {
		t.Fatalf("fence=%v", fence)
	}
}

func TestRollbackRestoresDataEngineTracker(t *testing.T) {
	dao := &dataEngineTrackedRollbackDAO{}
	dao.tracker.SetVersion(4)
	dao.tracker.SetSyncVersion(6)
	dao.tracker.MarkSync(1)
	tx := NewRollbackTx(RollbackUndo)
	if err := tx.captureDao(dao); err != nil {
		t.Fatal(err)
	}
	if err := dao.tracker.AcceptVersion(4, 5); err != nil {
		t.Fatal(err)
	}
	dao.tracker.IncSyncVersion()
	dao.tracker.MarkSync(2)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if dao.tracker.Version() != 4 || dao.tracker.SyncVersion() != 6 || dao.tracker.SyncDirtyMask() != 1 {
		t.Fatalf("tracker not restored: %+v", dao.tracker.Snapshot())
	}
}

func TestMarkPersistDeleteProducesDeleteMutation(t *testing.T) {
	p := &fakeMutationParticipant{}
	p.tracker.SetVersion(3)
	tx := NewRollbackTx(RollbackUndo)
	if err := tx.MarkPersistDelete(p); err != nil {
		t.Fatal(err)
	}
	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Mutations) != 1 || record.Mutations[0].Kind != dataengine.MutationDelete || record.Mutations[0].ExpectedVersion != 3 || record.Mutations[0].NextVersion != 4 {
		t.Fatalf("delete mutation=%+v", record.Mutations)
	}
}

// newAggregateTestDAO 是同一实体里的一个 DAO：带 tracker，按集合名区分文档。
type newAggregateTestDAO struct {
	coll    string
	tracker dataengine.Tracker
}

func (d *newAggregateTestDAO) Id() int64                         { return 7 }
func (d *newAggregateTestDAO) SetId(int64)                       {}
func (d *newAggregateTestDAO) DbName() string                    { return "game" }
func (d *newAggregateTestDAO) CollName() string                  { return d.coll }
func (d *newAggregateTestDAO) Dirty() entity.IDirty              { return &d.tracker }
func (d *newAggregateTestDAO) CleanDirty()                       { d.tracker.SelfClean() }
func (d *newAggregateTestDAO) DirtyTracker() *dataengine.Tracker { return &d.tracker }

func (d *newAggregateTestDAO) PrepareMutation(change PersistChange) (dataengine.Mutation, error) {
	version := d.tracker.Version()
	return dataengine.Mutation{
		Key:  dataengine.DocumentKey{Database: "game", Resource: d.coll, ID: 7},
		Kind: dataengine.MutationPut, ExpectedVersion: version, NextVersion: version + 1, Mask: change.Mask, Data: []byte{1},
	}, nil
}

func (d *newAggregateTestDAO) AcceptMutation(mutation dataengine.Mutation) error {
	return d.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

// newAggregateTestDAOs 模拟 entity.BuildEntity：三个 DAO 绑定为同一个聚合（按集合名排序），版本都是 0（新建实体）。
func newAggregateTestDAOs() (bag, profile, rank *newAggregateTestDAO) {
	bag, profile, rank = &newAggregateTestDAO{coll: "bag"}, &newAggregateTestDAO{coll: "profile"}, &newAggregateTestDAO{coll: "rank"}
	aggregate := entity.NewDaoAggregate(rank, bag, profile)
	for _, dao := range []*newAggregateTestDAO{bag, profile, rank} {
		dao.tracker.BindAggregate(aggregate)
	}
	return bag, profile, rank
}

// newAggregateTestEntity 是持有这三个 DAO 的实体，供 handler 内新建（CaptureCreatedEntity）的用例。
type newAggregateTestEntity struct {
	*entity.EntityBase
	daos []*newAggregateTestDAO
}

func (e *newAggregateTestEntity) Base() *entity.EntityBase { return e.EntityBase }

func (e *newAggregateTestEntity) RangeDao(f func(entity.DaoInterface)) {
	for _, dao := range e.daos {
		f(dao)
	}
}

func strictCommit(t *testing.T, mark func(tx *RollbackTx) error) CommitRecord {
	t.Helper()
	tx := NewRollbackTx(RollbackUndo)
	tx.durability = DurabilityStrict
	if err := mark(tx); err != nil {
		t.Fatal(err)
	}
	committer := &recordingCommitter{}
	if err := tx.durableCommit(nil, committer); err != nil {
		t.Fatal(err)
	}
	return committer.record
}

// 新建实体的第一笔持久提交只改了一个 DAO，也为每个 DAO 写整文档（冷加载要求聚合完整）；首笔提交被接受之后，
// 普通提交只写改过的 DAO，聚合标记为全部已持久，之后的登记不再逐个检查。
func TestNewEntityFirstPersistentCommitWritesEveryDAO(t *testing.T) {
	bag, profile, rank := newAggregateTestDAOs()
	record := strictCommit(t, func(tx *RollbackTx) error { return tx.MarkPersist(rank, 0b1) })
	got := make([]string, 0, len(record.Mutations))
	for _, mutation := range record.Mutations {
		got = append(got, mutation.Key.Resource)
		wantMask := dataengine.AllFields
		if mutation.Key.Resource == "rank" {
			wantMask = 0b1
		}
		if mutation.Kind != dataengine.MutationPut || mutation.ExpectedVersion != 0 || mutation.Mask != wantMask {
			t.Fatalf("first commit mutation %+v, want a full Put from version 0 with mask %#x", mutation, wantMask)
		}
	}
	if strings.Join(got, ",") != "rank,bag,profile" {
		t.Fatalf("first commit wrote %v, want the changed DAO then every other DAO of the new entity in collection order", got)
	}
	for _, dao := range []*newAggregateTestDAO{bag, profile, rank} {
		if dao.tracker.Version() != 1 {
			t.Fatalf("%s version=%d after the first commit, want 1", dao.coll, dao.tracker.Version())
		}
	}

	record = strictCommit(t, func(tx *RollbackTx) error { return tx.MarkPersist(rank, 0b1) })
	if len(record.Mutations) != 1 || record.Mutations[0].Key.Resource != "rank" {
		t.Fatalf("second commit wrote %+v, want only the changed DAO", record.Mutations)
	}
	if rank.tracker.Aggregate() != nil {
		t.Fatal("aggregate still checked after every DAO was stored")
	}
}

// 冷加载补空的 DAO（上线后新增、库里没有文档）版本为 0，其余 DAO 已持久：这个实体的下一笔持久提交只改了 profile，
// 也把补上的 rank 以整文档写出；已持久的 bag 不多写。
func TestColdLoadedEntityNextCommitWritesTheFilledDAO(t *testing.T) {
	bag, profile, rank := newAggregateTestDAOs()
	bag.tracker.SetVersion(4)
	profile.tracker.SetVersion(4)
	record := strictCommit(t, func(tx *RollbackTx) error { return tx.MarkPersist(profile, 0b1) })
	got := make(map[string]dataengine.Mutation, len(record.Mutations))
	for _, mutation := range record.Mutations {
		got[mutation.Key.Resource] = mutation
	}
	if len(got) != 2 || got["profile"].ExpectedVersion != 4 || got["rank"].ExpectedVersion != 0 || got["rank"].Kind != dataengine.MutationPut || got["rank"].Mask != dataengine.AllFields {
		t.Fatalf("commit after loading with a filled DAO wrote %+v, want profile from 4 and a full Put of rank from 0", record.Mutations)
	}
	if bag.tracker.Version() != 4 || rank.tracker.Version() != 1 {
		t.Fatalf("versions bag=%d rank=%d, want bag untouched at 4 and rank 1", bag.tracker.Version(), rank.tracker.Version())
	}
	record = strictCommit(t, func(tx *RollbackTx) error { return tx.MarkPersist(profile, 0b1) })
	if len(record.Mutations) != 1 || record.Mutations[0].Key.Resource != "profile" {
		t.Fatalf("commit after the filled DAO was stored wrote %+v, want only the changed DAO", record.Mutations)
	}
	if profile.tracker.Aggregate() != nil {
		t.Fatal("aggregate still checked after every DAO was stored")
	}
}

// 新建实体本身就落库：handler 内新建、没有改任何持久字段，CaptureCreatedEntity 也把每个 DAO 以整文档登记进事务。
func TestCapturedNewEntityWritesEveryDAOWithoutAnyChange(t *testing.T) {
	bag, profile, rank := newAggregateTestDAOs()
	created := &newAggregateTestEntity{EntityBase: entity.NewEntityBase(7, entity.EntityCategory(1), false), daos: []*newAggregateTestDAO{profile, rank, bag}}
	record := strictCommit(t, func(tx *RollbackTx) error { return tx.CaptureCreatedEntity(created, func() {}) })
	got := make([]string, 0, len(record.Mutations))
	for _, mutation := range record.Mutations {
		if mutation.Kind != dataengine.MutationPut || mutation.ExpectedVersion != 0 || mutation.Mask != dataengine.AllFields {
			t.Fatalf("creating commit mutation %+v, want a full Put from version 0", mutation)
		}
		got = append(got, mutation.Key.Resource)
	}
	if strings.Join(got, ",") != "bag,profile,rank" {
		t.Fatalf("creating commit wrote %v, want every DAO in collection order", got)
	}
}

// memory 快路径没有提交点：新建持久实体被拒绝（CreateInScope 撤销发布），非持久实体照常。
func TestMemoryHandlerRefusesToCreateAPersistentEntity(t *testing.T) {
	bag, profile, rank := newAggregateTestDAOs()
	persistent := &newAggregateTestEntity{EntityBase: entity.NewEntityBase(7, entity.EntityCategory(1), false), daos: []*newAggregateTestDAO{bag, profile, rank}}
	if err := (memoryHandlerCreates{}).CaptureCreatedEntity(persistent, func() {}); !errors.Is(err, ErrMemoryTransactionPersistentWrite) {
		t.Fatalf("memory handler creating a persistent entity: err=%v", err)
	}
	if err := (memoryHandlerCreates{}).CaptureCreatedEntity(newMockEntity(8, entity.EntityCategory(1)), func() {}); err != nil {
		t.Fatalf("memory handler creating a non-persistent entity: err=%v", err)
	}
}

// 回滚丢弃一并登记的 DAO 并恢复版本 0；下一笔提交仍是这个实体的第一笔，照样写齐。
func TestNewEntityAggregateRegistrationRollsBackWithTheTransaction(t *testing.T) {
	bag, profile, rank := newAggregateTestDAOs()
	tx := NewRollbackTx(RollbackUndo)
	if err := tx.MarkPersist(profile, 0b1); err != nil {
		t.Fatal(err)
	}
	if len(tx.participantOrder) != 3 {
		t.Fatalf("registered %d participants, want the whole new aggregate", len(tx.participantOrder))
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	record := strictCommit(t, func(tx *RollbackTx) error { return tx.MarkPersist(bag, 0b1) })
	if len(record.Mutations) != 3 {
		t.Fatalf("first commit after a rollback wrote %d DAO(s), want all 3", len(record.Mutations))
	}
	if rank.tracker.Version() != 1 {
		t.Fatalf("rank version=%d, want 1", rank.tracker.Version())
	}
}

// Remote 托管实体经同一份登记认领：新建实体（handler 内捕获，没改任何字段）的每个 DAO 都交给 Remote 聚合提交
// （整份 payload），不进普通 mutation。
func TestNewRemoteEntityFirstCommitClaimsEveryDAO(t *testing.T) {
	bag, profile, rank := newAggregateTestDAOs()
	created := &newAggregateTestEntity{EntityBase: entity.NewEntityBase(7, entity.EntityCategory(1), false), daos: []*newAggregateTestDAO{bag, profile, rank}}
	tx := NewRollbackTx(RollbackUndo)
	if err := tx.CaptureCreatedEntity(created, func() {}); err != nil {
		t.Fatal(err)
	}
	for _, dao := range []*newAggregateTestDAO{bag, profile, rank} {
		change, ok := tx.RemotePersistChangeFor(dao)
		if !ok || change.Delete || change.Mask != dataengine.AllFields {
			t.Fatalf("remote claim of %s = %+v ok=%v, want a whole-document write", dao.coll, change, ok)
		}
	}
	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Mutations) != 0 {
		t.Fatalf("claimed DAOs leaked into ordinary mutations: %+v", record.Mutations)
	}
}

func TestPersistFieldAvoidsPathsAndPreservesRevision(t *testing.T) {
	_, release := fctx.NewContext()
	defer release()
	if err := MarkPersistField(&fakeMutationParticipant{}, 1); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	tx := NewRollbackTx(RollbackUndo)
	p := &fakeMutationParticipant{}
	_, err := withRollbackTx(tx, func() (any, error) {
		for range 2 {
			if err := MarkPersistField(p, 1); err != nil {
				return nil, err
			}
		}
		return nil, MarkPersistPath(p, 1, dataengine.SyncField(1, dataengine.SyncField(1, nil)))
	})
	if err != nil {
		t.Fatal(err)
	}
	change := tx.participantChanges[p].Clone()
	if change.Mask != 1 || change.FullMask != 1 || change.Paths.Root() != nil || change.revision != 3 {
		t.Fatalf("full field must dominate paths without losing revision: %+v", change)
	}
}
