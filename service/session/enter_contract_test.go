package session

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"sync/atomic"
	"testing"
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

// U-0158 · C8 · RR-20260909-02：同一 RequestID 的落败者要撤回自己的 run 与 claim。撤回中途，同一
// owner 的另一次 Enter 合法地进入并拿到新 claim；落败者剩下的撤回不得碰到它。此前先删 run 再删
// claim：run 一没，新入者把孤立 claim 当过期清掉、建新 run 与新 claim，而删后重建的键版本又回到
// 1，落败者随后按版本删 claim 就把新入者的删了——owner 再进一次又成功，两个 run 同时 open。
// 现在先释放 claim 再删 run：claim 指向的 run 还 open 时没有人能合法替换它，版本比较才成立。
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

// U-0154 · C8 · RR-20260908-01：两个 owner 并发用同一个 RequestID 进入。claim 只串行化"每个 owner
// 一个 live run"，串行化不了全局 RequestID；账本写入是这条请求唯一的仲裁点。落败者必须以
// ErrRequestInvalid 拒绝，并撤回自己刚建的 run 与 claim——它们从未交给调用方。此前账本
// Create 的 created=false 被丢弃，两个 owner 都拿到成功，落败者重试才发现请求"属于别人"。
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
