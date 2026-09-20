package global

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// contendedLeases loses its first compare-and-set on every Update: the mutate
// callback runs once against the current value, that attempt is discarded,
// and the real store re-reads and re-applies it — the retry the versionstore
// contract permits and kit's MemoryStore never exercises.
type contendedLeases struct {
	versionstore.Store[int32, GameLease]
}

func (c contendedLeases) Update(ctx context.Context, key int32, mutate versionstore.Mutate[GameLease]) (versionstore.Versioned[GameLease], bool, error) {
	current, found, err := c.Store.Get(ctx, key)
	if err != nil {
		return versionstore.Versioned[GameLease]{}, false, err
	}
	if _, _, err := mutate(current.Value, found); err != nil {
		return versionstore.Versioned[GameLease]{}, false, err
	}
	return c.Store.Update(ctx, key, mutate)
}

// The incarnation is minted once per acquire, however many times the
// compare-and-set retries: a minter that is rate-limited or remote is paid
// per token, and a token minted inside the callback is minted per attempt.
func TestAcquireMintsOneIncarnationAcrossCompareAndSetRetries(t *testing.T) {
	minted := 0
	service, _ := newService(t, func(cfg *Config) {
		cfg.Leases = contendedLeases{versionstore.NewMemoryStore[int32, GameLease]()}
		cfg.NewIncarnation = func() (string, error) { minted++; return "inc-" + strings.Repeat("x", minted), nil }
	})
	bind(t, service, 7, 100)
	lease, err := service.AcquireLease(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if minted != 1 {
		t.Fatalf("one acquire that retried its compare-and-set minted %d incarnations", minted)
	}
	if lease.Incarnation != "inc-x" {
		t.Fatalf("the stored lease carries %q, not the one token that was minted", lease.Incarnation)
	}
}

// A refused renewal names the game, never the holder's token: a caller that
// does not hold the lease must not learn the one string that would let it
// renew. The refusal text is the only channel that could leak it.
func TestANotHolderRefusalDoesNotRevealTheLiveIncarnation(t *testing.T) {
	service, _ := newService(t, func(cfg *Config) {
		cfg.NewIncarnation = func() (string, error) { return "the-live-token-7f3a", nil }
	})
	ctx := context.Background()
	bind(t, service, 7, 100)
	if _, err := service.AcquireLease(ctx, 7); err != nil {
		t.Fatal(err)
	}
	_, err := service.RenewLease(ctx, 7, "a-token-from-a-dead-process", nil)
	if !errors.Is(err, ErrLeaseNotHolder) {
		t.Fatalf("renewal by a non-holder returned %v, want ErrLeaseNotHolder", err)
	}
	if strings.Contains(err.Error(), "the-live-token-7f3a") {
		t.Fatalf("the refusal reveals the live incarnation: %q", err.Error())
	}
	if err := service.ReleaseLease(ctx, 7, "a-token-from-a-dead-process"); err != nil {
		t.Fatalf("release by a non-holder is a counted no-op, got %v", err)
	}
}

// A retried CompleteMigration at the same epoch is a replay, and is reported
// as one. Counting it as an acceptance makes the migration rate read higher
// than the number of migrations, which is the number operators alarm on.
func TestARetriedCompletionIsReportedAsAReplay(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	service, _ := newService(t, func(cfg *Config) { cfg.Metrics = sink })
	ctx := context.Background()
	binding := bind(t, service, 7, 100)
	moving, err := service.BeginMigration(ctx, 7, 200, binding.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	done, err := service.CompleteMigration(ctx, 7, moving.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.CompleteMigration(ctx, 7, done.Epoch)
	if err != nil || again.Epoch != done.Epoch || again.GlobalSID != 200 {
		t.Fatalf("retried completion: err=%v binding=%+v", err, again)
	}
	if got := sink.Count("accepted:complete_migration"); got != 1 {
		t.Errorf("one migration completed reported %d accepts; %s", got, sink.Events())
	}
	if got := sink.Count("replayed:complete_migration"); got != 1 {
		t.Errorf("a retried completion reported %d replays; %s", got, sink.Events())
	}
}
