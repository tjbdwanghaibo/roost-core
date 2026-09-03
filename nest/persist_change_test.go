package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
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
		NestOptionWithWorkerNumAndMsgCap(1, 1, 16),
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
