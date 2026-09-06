package remoteentity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

func batchHarness(t *testing.T, kind entity.EntityKind, ids ...int64) (*remoteEntityManager, []int64) {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	guids := make([]int64, 0, len(ids))
	for _, id := range ids {
		live := newTestRemoteEntity(id, 1, kind)
		loader.add(live)
		guids = append(guids, live.GUId())
	}
	return mgr, guids
}

// A batch has one legal order: prepare → finalize → commit (or abort /
// indeterminate). Every step out of order must refuse with
// ErrRemoteCommitNotFinalized, otherwise a second finalize could rewrite the
// commits after the first was already handed to the WAL.
func TestRemoteWriteBatchRefusesStepsOutOfOrder(t *testing.T) {
	mgr, guids := batchHarness(t, 141, 2401)
	ctx := context.Background()
	batch, err := mgr.PrepareRemoteWriteBatch(ctx, guids)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := batch.Commit(ctx); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("Commit before FinalizeLocked = %v", err)
	}
	if err := batch.Indeterminate(ctx, errors.New("x")); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("Indeterminate before FinalizeLocked = %v", err)
	}
	if err := batch.FinalizeLocked(entity.RemoteTransactionOutcome{}); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("FinalizeLocked with a failed / zero outcome = %v", err)
	}
	outcome := entity.NewRemoteTransactionOutcome(remoteTestTxID(41), "write", "", true, 2)
	if err := batch.FinalizeLocked(outcome); err != nil {
		t.Fatal(err)
	}
	if err := batch.FinalizeLocked(outcome); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("second FinalizeLocked = %v", err)
	}
	if err := batch.Abort(ctx, errors.New("test complete")); err != nil {
		t.Fatal(err)
	}
	if err := batch.Indeterminate(ctx, errors.New("late")); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("Indeterminate after Abort = %v", err)
	}
}

// Admission limits: more entities than MaxWriteBatch is refused with both
// numbers, and a manager that has recorded a fatal release failure is fenced
// — it must not prepare new remote writes on ownership it can no longer
// prove it released.
func TestPrepareRemoteWriteBatchRefusesOversizeAndFencedManagers(t *testing.T) {
	mgr, guids := batchHarness(t, 142, 2411, 2412, 2413)
	mgr.cfg.MaxWriteBatch = 2
	_, err := mgr.PrepareRemoteWriteBatch(context.Background(), guids)
	if !errors.Is(err, entity.ErrRemoteOverloaded) || !strings.Contains(err.Error(), "batch=3 max=2") {
		t.Fatalf("oversize batch = %v", err)
	}
	mgr.cfg.MaxWriteBatch = 100
	mgr.recordReleaseFailure(errors.New("redis unreachable during unlock"))
	_, err = mgr.PrepareRemoteWriteBatch(context.Background(), guids[:1])
	if !errors.Is(err, entity.ErrRemoteFenced) || !strings.Contains(err.Error(), "redis unreachable") {
		t.Fatalf("fenced manager = %v", err)
	}
}

// Ownership fencing keys every lease by the owning sid; sid 0 would make
// every process look like the same owner.
func TestRemoteEntityModRequiresANonZeroSid(t *testing.T) {
	err := NewRemoteEntityMod(0).Init(viper.New())
	if err == nil || !strings.Contains(err.Error(), "non-zero sid is required") {
		t.Fatalf("Init with sid 0 = %v", err)
	}
}
