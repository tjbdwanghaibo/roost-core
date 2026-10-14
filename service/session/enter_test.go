// 会话 Enter 仲裁与撤回：RequestID 竞争的落败者撤回且不碰重新取得的 claim、账本写丢报错、Finish 重试与 Attach 释放、sweep 来源。

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type deleteHook struct {
	fired chan struct{}
	after func()
}

func (h *deleteHook) fire() {
	select {
	case <-h.fired:
		return
	default:
		close(h.fired)
		h.after()
	}
}

type runsWithDeleteHook struct {
	versionstore.Store[string, Run]
	hook *deleteHook
}

func (s *runsWithDeleteHook) Delete(ctx context.Context, key string, expect versionstore.Versioned[Run]) error {
	if err := s.Store.Delete(ctx, key, expect); err != nil {
		return err
	}
	s.hook.fire()
	return nil
}

type claimsWithDeleteHook struct {
	versionstore.Store[int64, Claim]
	hook *deleteHook
}

func (s *claimsWithDeleteHook) DeleteIf(ctx context.Context, key int64, expect versionstore.Versioned[Claim], match func(Claim) bool) error {
	store := s.Store.(versionstore.ConditionalDeleter[int64, Claim])
	if err := store.DeleteIf(ctx, key, expect, match); err != nil {
		return err
	}
	s.hook.fire()
	return nil
}

func (s *claimsWithDeleteHook) Delete(ctx context.Context, key int64, expect versionstore.Versioned[Claim]) error {
	if err := s.Store.Delete(ctx, key, expect); err != nil {
		return err
	}
	s.hook.fire()
	return nil
}

// 同一 RequestID 的落败者撤回自己的 run 与 claim，撤回中途同一 owner 的另一次 Enter 合法地拿到新 claim：落败者剩下的撤回不碰它。
// 先释放 claim 再删 run——claim 指向的 run 还 open 时没有人能合法替换它，版本比较才成立。
// 历史：RR-20260909-02。
func TestEnterCollisionCleanupNeverRemovesAReacquiredClaim(t *testing.T) {
	ctx := context.Background()
	ledger := &barrierLedger{Store: versionstore.NewMemoryStore[string, LedgerEntry](), ready: make(chan struct{})}
	hook := &deleteHook{fired: make(chan struct{})}
	runs := &runsWithDeleteHook{Store: versionstore.NewMemoryStore[string, Run](), hook: hook}
	claims := &claimsWithDeleteHook{Store: versionstore.NewMemoryStore[int64, Claim](), hook: hook}
	h := newHarness(t, func(cfg *Config) {
		cfg.Requests = ledger
		cfg.Runs = runs
		cfg.Claims = claims
	})

	var replacement Run
	var replacementErr error
	hook.after = func() {
		// The loser has undone one write. Its owner enters again with a new
		// request — a legitimate re-entry that must survive the rest of the undo.
		// The hook runs on the loser's goroutine after the ledger named the
		// winner, so the loser is whichever owner the ledger does not name.
		entry, _, _ := ledger.Store.Get(ctx, "collision-request")
		loserOwner := int64(1)
		if entry.Value.OwnerID == 1 {
			loserOwner = 2
		}
		replacement, replacementErr = h.service.Enter(ctx, loserOwner, enterReq("replacement-request"))
	}

	type result struct {
		owner int64
		run   Run
		err   error
	}
	done := make(chan result, 2)
	for _, owner := range []int64{1, 2} {
		go func(id int64) {
			run, err := h.service.Enter(ctx, id, enterReq("collision-request"))
			done <- result{id, run, err}
		}(owner)
	}
	a, b := <-done, <-done
	if (a.err == nil) == (b.err == nil) {
		t.Fatalf("expected exactly one collision winner: owner %d → %v / owner %d → %v", a.owner, a.err, b.owner, b.err)
	}
	if !errors.Is(errors.Join(a.err, b.err), ErrRequestInvalid) {
		t.Fatalf("loser was not refused with ErrRequestInvalid: %v / %v", a.err, b.err)
	}
	loser := a
	if a.err == nil {
		loser = b
	}
	if replacementErr != nil || replacement.ID == "" {
		t.Fatalf("the re-entry during cleanup failed: run=%+v err=%v", replacement, replacementErr)
	}
	if replacement.OwnerID != loser.owner {
		t.Fatalf("re-entry ran for owner %d, want the loser %d", replacement.OwnerID, loser.owner)
	}
	claim, found, err := claims.Store.Get(ctx, loser.owner)
	if err != nil {
		t.Fatal(err)
	}
	if !found || claim.Value.RunID != replacement.ID {
		third, thirdErr := h.service.Enter(ctx, loser.owner, enterReq("third-request"))
		t.Fatalf("the re-entry's claim was removed by the loser's cleanup (found=%v claim=%+v); a third Enter then answered (%s, %v) instead of ErrAlreadyRunning",
			found, claim.Value, third.ID, thirdErr)
	}
	if _, err := h.service.Enter(ctx, loser.owner, enterReq("third-request")); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("third Enter while the re-entry's run is open = %v, want ErrAlreadyRunning", err)
	}
	if current, ok, err := h.service.Current(ctx, loser.owner); err != nil || !ok || current.ID != replacement.ID {
		t.Fatalf("Current(loser) = (%s, %v, %v), want the re-entry's run %s", current.ID, ok, err, replacement.ID)
	}
	if _, ok, _ := runs.Store.Get(ctx, loser.run.ID); ok && loser.run.ID != "" {
		t.Fatalf("the loser's undelivered run %s is still stored", loser.run.ID)
	}
}

type barrierLedger struct {
	versionstore.Store[string, LedgerEntry]
	reads atomic.Int32
	ready chan struct{}
}

func (s *barrierLedger) Get(ctx context.Context, key string) (versionstore.Versioned[LedgerEntry], bool, error) {
	value, ok, err := s.Store.Get(ctx, key)
	if s.reads.Add(1) == 2 {
		close(s.ready)
	}
	if s.reads.Load() <= 2 {
		<-s.ready
	}
	return value, ok, err
}

// 两个 owner 并发用同一个 RequestID 进入：账本写入是这条请求唯一的仲裁点，落败者以 ErrRequestInvalid 拒绝，并撤回自己刚建的 run 与
// claim——它们从未交给调用方。
// 历史：RR-20260908-01。
func TestEnterRefusesTheLoserOfARequestIDRaceAndUndoesItsRun(t *testing.T) {
	ctx := context.Background()
	ledger := &barrierLedger{Store: versionstore.NewMemoryStore[string, LedgerEntry](), ready: make(chan struct{})}
	h := newHarness(t, func(cfg *Config) { cfg.Requests = ledger })
	type result struct {
		owner int64
		run   Run
		err   error
	}
	done := make(chan result, 2)
	for _, owner := range []int64{1, 2} {
		go func(id int64) {
			run, err := h.service.Enter(ctx, id, enterReq("same-request"))
			done <- result{id, run, err}
		}(owner)
	}
	a, b := <-done, <-done
	entry, found, err := ledger.Store.Get(ctx, "same-request")
	if err != nil || !found {
		t.Fatalf("ledger after the race: found=%v err=%v", found, err)
	}
	winner, loser := a, b
	if loser.owner == entry.Value.OwnerID {
		winner, loser = b, a
	}
	if winner.err != nil || winner.run.ID != entry.Value.RunID {
		t.Fatalf("winner (owner %d) = (%+v, %v); ledger names run %s", winner.owner, winner.run, winner.err, entry.Value.RunID)
	}
	if !errors.Is(loser.err, ErrRequestInvalid) {
		t.Fatalf("loser (owner %d) = (%+v, %v), want ErrRequestInvalid", loser.owner, loser.run, loser.err)
	}
	if _, held, _ := h.service.Current(ctx, loser.owner); held {
		t.Fatalf("the loser still holds a run after being refused")
	}
	if _, found, _ := h.service.cfg.Claims.Get(ctx, loser.owner); found {
		t.Fatal("the loser's claim was not released")
	}
	if _, err := h.service.Enter(ctx, loser.owner, enterReq("same-request")); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("loser's retry = %v, want the same refusal", err)
	}
	if replay, err := h.service.Enter(ctx, winner.owner, enterReq("same-request")); err != nil || replay.ID != winner.run.ID {
		t.Fatalf("winner's retry = (%+v, %v), want a replay of run %s", replay, err, winner.run.ID)
	}
}

// ledgerThatFailsOnce is the request ledger with its first write lost — the Redis round trip that did not come back. Enter
// writes the ledger through Update (the RequestID's compare-and-set); Create is covered too so the fake does not depend on
// which write the service uses.
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

// Enter's last step writes the replay ledger. When that write fails the run already exists and the claim already names it,
// so Enter reports the error rather than answering success with a run the ledger has never heard of: the caller then retries,
// finds its own claim and is refused — which is correct, because the run really is open.
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

func TestAttachCannotAssertAlreadyReleased(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "forge")
	resource := scene("held")
	resource.ReleasedAtUnix = h.clock.Now().Unix()
	resource.ForcedRelease = true
	if _, err := h.service.Attach(ctx, 1, run.ID, resource); !errors.Is(err, ErrRunInvalid) {
		t.Fatalf("forged release markers accepted: %v", err)
	}
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("held")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "done"); err != nil {
		t.Fatal(err)
	}
	if h.releaser.count("scene", "held") != 1 {
		t.Fatalf("caller-controlled release marker skipped release: calls=%d", h.releaser.count("scene", "held"))
	}
}

func TestSuccessfulFinishRetryFreesClaim(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "retry")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("retry")); err != nil {
		t.Fatal(err)
	}
	h.releaser.failFor["scene:retry"] = 1
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "done"); err == nil {
		t.Fatal("expected first release failure")
	}
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "done"); err != nil {
		t.Fatal(err)
	}
	_, found, err := h.claims.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("successful Finish retry leaves terminal owner's claim")
	}
}

type pausedClaimDelete struct {
	ClaimStore
	once             atomic.Bool
	entered, release chan struct{}
}

func (s *pausedClaimDelete) DeleteIf(ctx context.Context, k int64, v versionstore.Versioned[Claim], match func(Claim) bool) error {
	if s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.ClaimStore.DeleteIf(ctx, k, v, match)
}
func TestTerminalFinishCannotDeleteReacquiredClaim(t *testing.T) {
	gate := &pausedClaimDelete{ClaimStore: versionstore.NewMemoryStore[int64, Claim](), entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, func(c *Config) {
		if addr := os.Getenv("ROOST_REVIEW_REDIS"); addr != "" {
			client, err := driver.NewClient(fredis.DefaultConfig(addr))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			stores, err := NewRedisStores(client, RedisConfig{Prefix: redistest.Prefix(t, addr, "service-review-b"), RequestTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			c.Runs = stores.Runs
			c.Requests = stores.Requests
			gate.ClaimStore = stores.Claims
			t.Log("real Redis backend")
		}
		c.Claims = gate
	})
	ctx := context.Background()
	a := mustEnter(t, h, 1, "a")
	done := make(chan error, 1)
	go func() { _, err := h.service.Finish(ctx, 1, a.ID, StateSucceeded, "done"); done <- err }()
	<-gate.entered
	b, bErr := h.service.Enter(ctx, 1, enterReq("b"))
	close(gate.release)
	aErr := <-done
	if bErr != nil || aErr != nil {
		t.Fatal(bErr, aErr)
	}
	c, cErr := h.service.Enter(ctx, 1, enterReq("c"))
	bStored, bFound, err := h.service.cfg.Runs.Get(ctx, b.ID)
	if err != nil || !bFound {
		t.Fatal(err)
	}
	if cErr == nil && bStored.Value.Live(h.clock.Now().Unix()) {
		t.Fatalf("late Finish deleted replacement claim: b=%s c=%s both live", b.ID, c.ID)
	}
}

func TestConfiguredOwnerSourceReleasesExpiredRun(t *testing.T) {
	calls := 0
	h := newHarness(t, func(c *Config) {
		c.Owners = OwnerSourceFunc(func(ctx context.Context, limit int) ([]int64, error) {
			calls++
			if limit != 1 {
				t.Fatalf("limit=%d", limit)
			}
			return []int64{7}, nil
		})
	})
	ctx := context.Background()
	run := mustEnter(t, h, 7, "idle")
	if _, err := h.service.Attach(ctx, 7, run.ID, scene("idle")); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(11 * time.Minute)
	resolved, err := h.service.SweepPending(ctx, 1)
	if err != nil || calls != 1 || len(resolved) != 1 || resolved[0].State != StateExpired || h.releaser.count("scene", "idle") != 1 {
		t.Fatalf("resolved=%+v calls=%d releases=%d err=%v", resolved, calls, h.releaser.count("scene", "idle"), err)
	}
	if _, found, err := h.claims.Get(ctx, 7); found || err != nil {
		t.Fatal(found, err)
	}
}

func TestOwnerSourceIsBoundedAndFailuresAreRetryable(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("roster unavailable")
	attempt := 0
	h := newHarness(t, func(c *Config) {
		c.Owners = OwnerSourceFunc(func(context.Context, int) ([]int64, error) {
			attempt++
			if attempt == 1 {
				return nil, sentinel
			}
			return []int64{1, 2}, nil
		})
	})
	run := mustEnter(t, h, 1, "live")
	if _, err := h.service.SweepPending(ctx, 1); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err := h.service.SweepPending(ctx, 1); !errors.Is(err, ErrRangeInvalid) {
		t.Fatal(err)
	}
	stored, _, err := h.runs.Get(ctx, run.ID)
	if err != nil || stored.Value.State != StateOpen {
		t.Fatal(stored, err)
	}
	lazy := newHarness(t)
	if lazy.service.BackgroundSweepEnabled() {
		t.Fatal("unconfigured source enabled")
	}
	if result, err := lazy.service.SweepPending(ctx, 1); len(result) != 0 || err != nil {
		t.Fatal(result, err)
	}
}
