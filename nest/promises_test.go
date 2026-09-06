package nest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// otherKindEntity is a second concrete entity type so a Cast can ask for the
// wrong one.
type otherKindEntity struct {
	*entity.EntityBase
}

func (m *otherKindEntity) Base() *entity.EntityBase { return m.EntityBase }

type promiseParticipant struct{ calls int }

func (p *promiseParticipant) PrepareCommit(*RollbackTx) error { p.calls++; return nil }

// shortGetter returns fewer entities than asked, the way a getter with a
// bug (or a partial remote answer) would.
type shortGetter struct{ *mockGetter }

func (g shortGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	es, err := g.mockGetter.GetMany(ctx, ids, categories)
	if err != nil || len(es) == 0 {
		return es, err
	}
	return es[:len(es)-1], nil
}

func castFixture(t *testing.T, getter entity.Getter) (playerID, allianceID, otherID int64) {
	t.Helper()
	withCastGroupFunc(t)
	bindCastGetter(t, getter)
	playerID = mustBuildCastID(t, 100, castPlayerCategory, castPlayerKind)
	allianceID = mustBuildCastID(t, 200, castAllianceCategory, castAllianceKind)
	otherID = mustBuildCastID(t, 300, castOtherCategory, castOtherKind)
	return
}

// The typed Cast helpers promise a typed result or ErrCastTypeMismatch that
// names the id and the actual type. A wrong assertion that fell through would
// hand the handler a zero entity to write into.
func TestTypedCastsRefuseTheWrongEntityType(t *testing.T) {
	getter := newMockGetter()
	playerID, allianceID, otherID := castFixture(t, getter)
	player := newMockEntityWithKind(playerID, castPlayerCategory, castPlayerKind)
	getter.Add(player)
	getter.Add(newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind))
	getter.Add(newMockEntityWithKind(otherID, castOtherCategory, castOtherKind))
	_, release := entity.NewGuardScope("cast_test")
	defer release()
	if !entity.GetEntityGuard().RequireEntity(player) {
		t.Fatal("lock player")
	}

	if _, err := CastTargetOne[*otherKindEntity](NewCastTarget(allianceID)); !errors.Is(err, ErrCastTypeMismatch) || !strings.Contains(err.Error(), "*nest.mockEntity") {
		t.Fatalf("CastTargetOne wrong type = %v", err)
	}
	if _, _, err := CastTwo[*mockEntity, *otherKindEntity](NewCastTarget(allianceID), NewCastTarget(otherID)); !errors.Is(err, ErrCastTypeMismatch) {
		t.Fatalf("CastTwo second wrong = %v", err)
	}
	if _, _, err := CastTwo[*otherKindEntity, *mockEntity](NewCastTarget(allianceID), NewCastTarget(otherID)); !errors.Is(err, ErrCastTypeMismatch) {
		t.Fatalf("CastTwo first wrong = %v", err)
	}
	// CastThree needs a third same-order target; reuse other twice is a
	// deadlock-risk shape, so only the first-position mismatch is exercised.
	if _, _, _, err := CastThree[*otherKindEntity, *mockEntity, *mockEntity](NewCastTarget(allianceID), NewCastTarget(otherID), NewCastTarget(otherID)); err == nil {
		t.Fatal("CastThree with a wrong first type must not succeed")
	}
	if _, err := CastMulti(); !errors.Is(err, ErrCastInvalidTarget) || !strings.Contains(err.Error(), "empty targets") {
		t.Fatalf("CastMulti() = %v", err)
	}
	if _, err := CastMulti(NewCastTarget(allianceID), CastTarget{}); !errors.Is(err, ErrCastInvalidTarget) || !strings.Contains(err.Error(), "index=1 id=0") {
		t.Fatalf("CastMulti with a zero id = %v", err)
	}
}

// A getter that answers with the wrong number of entities is a broken
// contract, not "some were missing": the positions could no longer be
// matched to targets.
func TestCastMultiRefusesAGetterThatReturnsTheWrongCount(t *testing.T) {
	base := newMockGetter()
	playerID, allianceID, otherID := castFixture(t, shortGetter{base})
	player := newMockEntityWithKind(playerID, castPlayerCategory, castPlayerKind)
	base.Add(player)
	base.Add(newMockEntityWithKind(allianceID, castAllianceCategory, castAllianceKind))
	base.Add(newMockEntityWithKind(otherID, castOtherCategory, castOtherKind))
	_, release := entity.NewGuardScope("cast_test")
	defer release()
	if !entity.GetEntityGuard().RequireEntity(player) {
		t.Fatal("lock player")
	}
	_, err := CastMulti(NewCastTarget(allianceID), NewCastTarget(otherID))
	if !errors.Is(err, ErrCastInvalidTarget) || !strings.Contains(err.Error(), "returned 1 entities for 2") {
		t.Fatalf("CastMulti with a short getter = %v", err)
	}
}

// An engine is single-use: Start after Shutdown must refuse, and Start
// without a getter must refuse before any worker pool exists.
func TestEngineStartRefusesAfterShutdownAndWithoutGetter(t *testing.T) {
	if err := NewEngine().Start(); !errors.Is(err, ErrGetterNotSet) {
		t.Fatalf("Start without getter = %v", err)
	}
	mgr := NewEngine(NestOptionWithGetter(newMockGetter()))
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(); !errors.Is(err, ErrNestStopped) {
		t.Fatalf("Start after Shutdown = %v", err)
	}
}

// A committed or rolled-back transaction accepts nothing more; undo owners,
// tokens and participants must be comparable because they key a map; a
// mutation may not mix the canonical and the legacy identity forms.
func TestRollbackTxRefusesLateAndUncomparableRegistrations(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	owner := &rollbackTestDao{}
	if err := tx.RecordUndo(owner, 1, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := tx.RecordUndo([]int{1}, 1, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "undo owner is not comparable") {
		t.Fatalf("slice owner = %v", err)
	}
	if err := tx.RecordUndoToken(owner, 1, []int{1}, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "undo token is not comparable") {
		t.Fatalf("slice token = %v", err)
	}
	if err := tx.RecordUndo(nil, 1, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "invalid undo operation") {
		t.Fatalf("nil owner = %v", err)
	}
	if err := tx.AddMutation(EntityMutation{Key: dataengine.DocumentKey{Database: "g", Resource: "r", ID: 1}, EntityID: 1, Kind: dataengine.MutationPut, NextVersion: 1, Data: []byte("x")}); !errors.Is(err, dataengine.ErrMixedMutationForms) {
		t.Fatalf("mixed mutation forms = %v", err)
	}
	participant := &promiseParticipant{}
	if err := tx.RegisterCommitParticipant(participant); err != nil {
		t.Fatal(err)
	}
	if err := tx.RegisterCommitParticipant(participant); err != nil || len(tx.participants) != 1 {
		t.Fatalf("re-registering the same participant: err=%v participants=%d, want dedupe", err, len(tx.participants))
	}
	tx.Commit()
	if err := tx.RecordUndo(owner, 2, func() error { return nil }); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("RecordUndo after Commit = %v", err)
	}
	if err := tx.RegisterCommitParticipant(&promiseParticipant{}); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("RegisterCommitParticipant after Commit = %v", err)
	}
	if err := tx.AddMutation(EntityMutation{}); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("AddMutation after Commit = %v", err)
	}
}
