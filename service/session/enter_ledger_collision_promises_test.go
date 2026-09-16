package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// barrierLedger lets both initial ledger reads finish before either caller
// writes, so two owners racing one RequestID both see "absent". Writes go to
// the real memory store, whose compare-and-set decides the winner.
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
