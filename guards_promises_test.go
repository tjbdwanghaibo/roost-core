package remoteentity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// countingEval records how many Lua transitions reached Redis so a refusal
// that must be decided locally can prove it never touched the store.
type countingEval struct {
	inner *markerEvalStub
	calls int
}

func (c *countingEval) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	c.calls++
	return c.inner.Eval(ctx, script, keys, args...)
}

func expectMarkerErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// U-0093 (C2): the ownership marker's local argument checks and the CAS
// conflict translations each refuse for the reason they name; the local
// checks must not spend a round trip.
func TestRedisMarkerRefusesEachInvalidTransitionArgument(t *testing.T) {
	eval := &countingEval{inner: newMarkerEvalStub()}
	store := newRedisMarkerForEval(eval, "marks")
	ctx := context.Background()

	_, err := store.ClaimOwnership(ctx, 0, 1001)
	expectMarkerErr(t, err, "invalid ownership claim for 0")
	_, err = store.ClaimOwnership(ctx, 42, 0)
	expectMarkerErr(t, err, "invalid ownership claim for 42")
	_, err = store.EnterSharedExpected(ctx, 42, entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: 1, RouteEpoch: 1, Shared: true})
	expectMarkerErr(t, err, "expected local lease for 42")
	_, err = store.LeaveSharedExpected(ctx, 42, entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: 2, RouteEpoch: 1})
	expectMarkerErr(t, err, "invalid shared lease for 42")
	_, err = store.LeaveSharedExpected(ctx, 42, entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: 0, RouteEpoch: 1, Shared: true})
	expectMarkerErr(t, err, "invalid shared lease for 42")
	if eval.calls != 0 {
		t.Fatalf("locally refused transitions reached Redis %d times", eval.calls)
	}

	lease, err := store.ClaimOwnership(ctx, 42, 1001)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ClaimOwnership(ctx, 42, 2002)
	expectMarkerErr(t, err, "ownership claim conflict for 42")
	stale := lease
	stale.MarkerEpoch++
	_, err = store.EnterSharedExpected(ctx, 42, stale)
	expectMarkerErr(t, err, "enter shared compare-and-swap failed for 42")
	shared, err := store.EnterSharedExpected(ctx, 42, lease)
	if err != nil || !shared.Shared {
		t.Fatalf("enter shared = %#v, %v", shared, err)
	}
	_, err = store.LeaveSharedExpected(ctx, 42, entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: shared.MarkerEpoch + 1, RouteEpoch: 1, Shared: true})
	if err == nil {
		t.Fatal("leave shared with a stale lease must fail")
	}
	observed, found, err := store.GetOwnership(ctx, 42)
	if err != nil || !found || observed != shared {
		t.Fatalf("ownership after refused transitions = %#v %v %v", observed, found, err)
	}
}

// renewIfNeeded must reject an interest whose consumer, key, or expiry is
// unusable instead of registering it — a bad interest would otherwise keep a
// snapshot fanning out to nobody.
func TestRemoteInterestRegistryRejectsUnusableInterest(t *testing.T) {
	const kind entity.EntityKind = 134
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1904, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	future := time.Now().Add(time.Second).UnixNano()
	cases := map[string]entity.RemoteSnapshotInterest{
		"consumer zero": {ConsumerSID: 0, Key: key, ExpiresAt: future},
		"key kind mismatch": {ConsumerSID: 7, Key: entity.RemoteSnapshotKey{EntityID: id, Kind: kind + 1, Scope: 1}, ExpiresAt: future},
		"key raw id": {ConsumerSID: 7, Key: entity.RemoteSnapshotKey{EntityID: 1904, Kind: kind, Scope: 1}, ExpiresAt: future},
		"already expired": {ConsumerSID: 7, Key: key, ExpiresAt: time.Now().Add(-time.Millisecond).UnixNano()},
	}
	for name, interest := range cases {
		t.Run(name, func(t *testing.T) {
			registry := newRemoteInterestRegistry()
			renew, err := registry.renewIfNeeded(interest, 0)
			if !errors.Is(err, entity.ErrRemoteRejected) || renew {
				t.Fatalf("renewIfNeeded = %v, %v; want ErrRemoteRejected", renew, err)
			}
			if registry.interested(interest.Key) || registry.total != 0 {
				t.Fatal("a rejected interest was registered")
			}
		})
	}
	var nilRegistry *remoteInterestRegistry
	if _, err := nilRegistry.renewIfNeeded(entity.RemoteSnapshotInterest{ConsumerSID: 7, Key: key, ExpiresAt: future}, 0); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("nil registry err = %v", err)
	}
}

type plainStorageBackend struct{}

func (plainStorageBackend) CommitRemote(context.Context, entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	return entity.RemoteCommitReceipt{}, nil
}
func (plainStorageBackend) CommitStatus(context.Context, entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	return entity.RemoteCommitStatus{}, nil
}
func (plainStorageBackend) CommitRemoteBatch(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}
func (plainStorageBackend) LoadRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	return entity.RemoteSnapshotEnvelope{}, false, nil
}
func (plainStorageBackend) PendingRemoteCommits(context.Context, int) ([]entity.RemoteCommitStatus, error) {
	return nil, nil
}
func (plainStorageBackend) MarkRemoteCommitPublished(context.Context, entity.RemoteTransactionID) error {
	return nil
}
func (plainStorageBackend) EnsureRemoteStorage(context.Context) error { return nil }

type nopLoader struct{}

func (nopLoader) LoadRemoteEntity(context.Context, int64, entity.EntityKind) (entity.IThreadSafeRemoteEntity, error) {
	return nil, nil
}

// The backend needs both halves, and a storage that cannot run a transaction
// must say so rather than commit the batch piecewise.
func TestBackendRequiresBothHalvesAndAtomicStoreForTransactions(t *testing.T) {
	if _, err := NewBackend(nil, plainStorageBackend{}); err == nil {
		t.Fatal("backend without loader accepted")
	}
	if _, err := NewBackend(nopLoader{}, nil); err == nil {
		t.Fatal("backend without storage accepted")
	}
	backend, err := NewBackend(nopLoader{}, plainStorageBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyRemoteCommitsInTransaction(context.Background(), []entity.RemoteCommit{{}}); !errors.Is(err, entity.ErrRemoteAtomicBatchUnsupported) {
		t.Fatalf("non-atomic storage err = %v, want ErrRemoteAtomicBatchUnsupported", err)
	}
}
