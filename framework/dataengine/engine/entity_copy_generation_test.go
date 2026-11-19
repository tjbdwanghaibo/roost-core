package engine

import (
	"context"
	"errors"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

type noCopySession struct{ fmongo.IMongo }

func (noCopySession) StartSession(context.Context) (fmongo.ISession, error) {
	return nil, errors.New("test: session not opened")
}

func TestRejectedCopyDoesNotAuthorizeCleanup(t *testing.T) {
	ctx := context.Background()
	cl := mongotest.NewClient()
	src := newCopyServer(t, cl, 3)
	id := seedPreload(t, cl, 3, preloadFirstKind, "preload_first", 987, 1, nil)[0]
	dst := cl.Collection(testDatabase+"_4", "preload_first")
	if err := dst.Seed(bson.M{"_id": id, "_version": int64(3), "_last_tx": "different"}); err != nil {
		t.Fatal(err)
	}
	src.runtime.Projector.holdEntityHandOff(id)
	src.store.client = noCopySession{cl}
	r, err := src.runtime.CopyForTransfer(ctx, id, 4, true, CopyFence{Epoch: 4, ID: "first"})
	if err == nil || len(r.Documents) != 0 {
		t.Fatalf("failed transaction startup returned %+v, %v", r, err)
	}
	if _, ok := dst.Lookup(id); !ok {
		t.Fatal("target removed")
	}
}

func TestUnknownCopyCannotDeleteDifferentTransactionOrTombstone(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit bson.M
	}{{"last tx", bson.M{"_last_tx": "other"}}, {"tombstone", bson.M{"_deleted": true}}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cl := mongotest.NewClient()
			src := newCopyServer(t, cl, 3)
			id := seedPreload(t, cl, 3, preloadFirstKind, "preload_first", 988, 1, nil)[0]
			src.runtime.Projector.holdEntityHandOff(id)
			src.store.client = lostReplyClient{cl}
			r, err := src.runtime.CopyForTransfer(ctx, id, 4, true, CopyFence{Epoch: 4, ID: "first"})
			if !errors.Is(err, fmongo.ErrCommitResultUnknown) || len(r.Documents) != 1 {
				t.Fatalf("unknown: %+v %v", r, err)
			}
			src.store.client = cl
			dst := cl.Collection(testDatabase+"_4", "preload_first")
			if _, err := dst.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": tc.edit}); err != nil {
				t.Fatal(err)
			}
			if n, err := src.runtime.DropEntityCopy(ctx, r, 4); err != nil || n != 0 {
				t.Fatalf("drop=%d %v", n, err)
			}
			if _, ok := dst.Lookup(id); !ok {
				t.Fatal("unrelated target removed")
			}
		})
	}
}

// Both former hosts can still remove their matching copies without claiming C's binding.
func TestStartupReconcileAfterTwoTransfers(t *testing.T) {
	ctx := context.Background()
	cl := mongotest.NewClient()
	a, b := newCopyServer(t, cl, 3), newCopyServer(t, cl, 4)
	id := seedPreload(t, cl, 3, preloadFirstKind, "preload_first", 990, 1, nil)[0]
	a.runtime.Projector.holdEntityHandOff(id)
	b.runtime.Projector.holdEntityHandOff(id)
	if _, err := a.runtime.CopyForTransfer(ctx, id, 4, true, CopyFence{4, "A-B"}); err != nil {
		t.Fatal(err)
	}
	bc, err := b.runtime.CopyForTransfer(ctx, id, 5, true, CopyFence{5, "B-C"})
	if err != nil {
		t.Fatal(err)
	}
	ac, err := a.runtime.CopyForTransfer(ctx, id, 5, false, CopyFence{5, "startup-check"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := a.runtime.DropEntityCopy(ctx, ac, 3); err != nil || n != 1 {
		t.Fatalf("cleanup A: %d %v", n, err)
	}
	if n, err := b.runtime.DropEntityCopy(ctx, bc, 4); err != nil || n != 1 {
		t.Fatalf("cleanup B: %d %v", n, err)
	}
	if _, ok := cl.Collection(testDatabase+"_5", "preload_first").Lookup(id); !ok {
		t.Fatal("C lost the player")
	}
}

func TestCleanupFencedByReturnEvenForIdenticalDocuments(t *testing.T) {
	ctx := context.Background()
	cl := mongotest.NewClient()
	a, b := newCopyServer(t, cl, 3), newCopyServer(t, cl, 4)
	id := seedPreload(t, cl, 3, preloadFirstKind, "preload_first", 989, 1, nil)[0]
	a.runtime.Projector.holdEntityHandOff(id)
	b.runtime.Projector.holdEntityHandOff(id)
	out := CopyFence{Epoch: 4, ID: "out"}
	back := CopyFence{Epoch: 5, ID: "back"}
	if _, err := a.runtime.CopyForTransfer(ctx, id, 4, true, out); err != nil {
		t.Fatal(err)
	}
	// Recovery has already checked account and copied the old A snapshot. B now returns
	// the exact same payload before the last destructive step on A.
	receipt, err := a.runtime.CopyForTransfer(ctx, id, 4, false, out)
	if err != nil {
		t.Fatal(err)
	}
	returned, err := b.runtime.CopyForTransfer(ctx, id, 3, true, back)
	if err != nil || len(returned.Documents) != 1 || returned.Documents[0].Outcome != EntityCopyAlreadyPresent {
		t.Fatalf("identical return: %+v %v", returned, err)
	}
	if n, err := a.runtime.DropEntityCopy(ctx, receipt, 3); err != nil || n != 0 {
		t.Fatalf("stale receipt: %d %v", n, err)
	}
	if _, ok := cl.Collection(testDatabase+"_3", "preload_first").Lookup(id); !ok {
		t.Fatal("returned authoritative document removed")
	}
	if _, err := a.runtime.CopyForTransfer(ctx, id, 4, false, out); !errors.Is(err, ErrEntityCopyConflict) {
		t.Fatalf("stale copy after return: %v", err)
	}
	if n, err := b.runtime.DropEntityCopy(ctx, returned, 4); err != nil || n != 1 {
		t.Fatalf("current owner cleanup: %d %v", n, err)
	}
}
