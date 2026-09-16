package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// ledgerThatFailsOnce is the request ledger with its first write lost — the
// Redis round trip that did not come back. Enter writes the ledger through
// Update (RR-20260908-01 made it the RequestID's compare-and-set); Create is
// covered too so the fake does not depend on which write the service uses.
type ledgerThatFailsOnce struct {
	versionstore.Store[string, LedgerEntry]
	failed bool
}

func (l *ledgerThatFailsOnce) Create(ctx context.Context, key string, value LedgerEntry) (versionstore.Versioned[LedgerEntry], bool, error) {
	if !l.failed {
		l.failed = true
		return versionstore.Versioned[LedgerEntry]{}, false, fmt.Errorf("ledger: connection reset")
	}
	return l.Store.Create(ctx, key, value)
}

func (l *ledgerThatFailsOnce) Update(ctx context.Context, key string, mutate versionstore.Mutate[LedgerEntry]) (versionstore.Versioned[LedgerEntry], bool, error) {
	if !l.failed {
		l.failed = true
		return versionstore.Versioned[LedgerEntry]{}, false, fmt.Errorf("ledger: connection reset")
	}
	return l.Store.Update(ctx, key, mutate)
}

// Enter's last step writes the replay ledger. When that write fails the run
// already exists and the claim already names it, so Enter must report the
// error rather than answer success with a run the ledger has never heard of:
// the caller then retries, finds its own claim and is refused — which is
// correct, because the run really is open. Answering nil there was one of the
// six promises a temporary revert left every test green on (U-0017).
func TestEnterReportsALostLedgerWriteInsteadOfSuccess(t *testing.T) {
	h := newHarness(t, func(cfg *Config) {
		cfg.Requests = &ledgerThatFailsOnce{Store: versionstore.NewMemoryStore[string, LedgerEntry]()}
	})
	ctx := context.Background()

	run, err := h.service.Enter(ctx, 1, enterReq("req-1"))
	if err == nil {
		t.Fatal("Enter answered success although the replay ledger was never written")
	}
	if run.ID == "" {
		t.Fatal("Enter dropped the run it had already created; the caller can neither release nor find it")
	}
	if stored, ok, getErr := h.runs.Get(ctx, run.ID); getErr != nil || !ok || stored.Value.State != StateOpen {
		t.Fatalf("the run is not open in the store after a lost ledger write: ok=%v state=%v err=%v", ok, stored.Value.State, getErr)
	}
	// The retry is refused, not replayed: the ledger has no entry, the claim
	// is held, and one live run per owner is intact.
	if _, err := h.service.Enter(ctx, 1, enterReq("req-1")); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("retry after a lost ledger write returned %v, want ErrAlreadyRunning", err)
	}
	if got := h.metrics.Count("accepted:enter"); got != 0 {
		t.Fatalf("an enter whose ledger write failed was counted as accepted (%d); %s", got, h.metrics.Events())
	}
}
