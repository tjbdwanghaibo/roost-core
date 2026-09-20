package directory

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// contendedStore is a versionstore.Store whose Update loses its first
// compare-and-set: the mutate callback runs once against the current value,
// that attempt is discarded as if another writer got in between, and the
// real store then re-reads and re-applies it. That is the retry the
// versionstore contract permits ("Mutate may be called more than once"), and
// kit's MemoryStore — which the rest of this package's tests run on — never
// exercises it.
type contendedStore struct {
	versionstore.Store[string, Entry]
}

func (c contendedStore) Update(ctx context.Context, key string, mutate versionstore.Mutate[Entry]) (versionstore.Versioned[Entry], bool, error) {
	current, found, err := c.Store.Get(ctx, key)
	if err != nil {
		return versionstore.Versioned[Entry]{}, false, err
	}
	if _, _, err := mutate(current.Value, found); err != nil {
		return versionstore.Versioned[Entry]{}, false, err
	}
	return c.Store.Update(ctx, key, mutate)
}

// An operation is accepted once however many times its compare-and-set has
// to retry. Reporting from inside the mutate callback counts attempts, not
// outcomes, so under contention the accept rate reads higher than the number
// of reservations that exist — the metric operators size the directory by.
func TestAContendedWriteIsAcceptedOnce(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	dir, err := New(contendedStore{versionstore.NewMemoryStore[string, Entry]()}, Config{
		Normalize: NormalizeLower, DefaultTTL: time.Minute, Now: c.Now, Metrics: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	claim, err := dir.Reserve(ctx, "Alice", "acct-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:reserve"); got != 1 {
		t.Errorf("one reservation that retried its compare-and-set reported %d accepts; %s", got, sink.Events())
	}
	if _, err := dir.Commit(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:commit"); got != 1 {
		t.Errorf("one commit that retried its compare-and-set reported %d accepts; %s", got, sink.Events())
	}
	// Replays and refusals do not write, so they never retry — but they must
	// still be counted exactly once when the decision is made outside the
	// callback.
	if _, err := dir.Reserve(ctx, "Alice", "acct-1", 0); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("replayed:reserve"); got != 1 {
		t.Errorf("replayed:reserve = %d, want 1; %s", got, sink.Events())
	}
	if _, err := dir.Reserve(ctx, "Alice", "acct-2", 0); err == nil {
		t.Fatal("second owner reserved a committed key")
	}
	if got := sink.Count("refused:reserve:taken"); got != 1 {
		t.Errorf("refused:reserve:taken = %d, want 1; %s", got, sink.Events())
	}
}
