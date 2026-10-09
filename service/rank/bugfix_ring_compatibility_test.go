package rank

import (
	"context"
	"testing"
)

func TestLegacyRequestRingUpgradesWithoutDroppingPriorIDs(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	submit(t, s, 1, 10, UpdateSet, "")
	f.mu.Lock()
	f.hset(s.ownerKey(arena()), "1:applied", "old-a,old-b")
	f.mu.Unlock()
	submit(t, s, 1, 1, UpdateAdd, "new,comma")
	replay := submit(t, s, 1, 100, UpdateAdd, "old-b")
	if replay.Score.Value != 11 {
		t.Fatalf("legacy proof lost: %+v", replay)
	}
	replay = submit(t, s, 1, 100, UpdateAdd, "new,comma")
	if replay.Score.Value != 11 {
		t.Fatal(replay)
	}
	if err := s.Remove(ctx, arena(), 1); err != nil {
		t.Fatal(err)
	}
	fresh := submit(t, s, 1, 7, UpdateAdd, "new,comma")
	if fresh.Score.Value != 7 {
		t.Fatalf("removed owner retained ledger: %+v", fresh)
	}
}
func TestMalformedRequestRingFailsClosed(t *testing.T) {
	s, f := newStore(t)
	submit(t, s, 1, 10, UpdateSet, "")
	f.mu.Lock()
	f.hset(s.ownerKey(arena()), "1:applied_v2", "invalid-json")
	f.mu.Unlock()
	if _, err := s.Submit(context.Background(), arena(), Score{OwnerID: 1, Value: 5}, UpdateAdd, "retry"); err == nil {
		t.Fatal("malformed ledger silently discarded")
	}
	page, err := s.Page(context.Background(), arena(), 0, 1)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Score.Value != 10 {
		t.Fatal(page, err)
	}
}
