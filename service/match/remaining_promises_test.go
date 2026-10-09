package match

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"testing"
	"time"
)

func TestTicketTTLRejectsSubsecond(t *testing.T) {
	if _, err := NewStore(versionstore.NewMemoryStore[string, queueState](), Config{TicketTTL: time.Millisecond}); err == nil {
		t.Fatal("subsecond ticket TTL accepted despite Unix-second deadline")
	}
}

func TestCommitReplayReturnsOriginalMatchForSameTicketSet(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	a, b := enqueue(t, s, ranked(), player(1, 100), "a"), enqueue(t, s, ranked(), player(2, 100), "b")
	first, err := s.Commit(ctx, ranked(), []string{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Commit(ctx, ranked(), []string{b.ID, a.ID})
	if err != nil || again.ID != first.ID {
		t.Fatalf("same ticket set replay = %+v %v, want %s", again, err, first.ID)
	}
	c := enqueue(t, s, ranked(), player(3, 100), "c")
	if _, err := s.Commit(ctx, ranked(), []string{a.ID, c.ID}); err == nil {
		t.Fatal("different ticket set replayed a match")
	}
}
