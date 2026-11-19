package level

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 每个 hit 请求一个动作，每个 action.done 给 done 加一；leave 结束阶段 a（取消它没完成的动作）。同时最多 2 个动作。
func ledgerProgram(t *testing.T) *Program {
	t.Helper()
	d := Definition{Version: 1, Name: "ledger", Revision: 1, TimeDomain: "tick", Initial: []string{"a"}, Limits: Limits{Steps: 64, Timers: 1, Effects: 2, Sources: 1},
		Variables: []Variable{{Scope: Global, Name: "done", Default: Int(0), Public: true}},
		Stages: []StageDefinition{{ID: "a", Triggers: []TriggerDefinition{
			{ID: "ask", Event: "hit", Actions: []Action{{Kind: "request", Name: "spawn"}}},
			{ID: "count", Event: EventActionDone, Actions: []Action{{Kind: "add", Scope: Global, Name: "done", Value: Literal(Int(1))}}},
			{ID: "leave", Event: "leave", Actions: []Action{{Kind: "start", Name: "b"}, {Kind: "end", Name: "a"}}},
		}}, {ID: "b"}}}
	return compiled(t, d, Registry{Events: map[string]map[string]ValueKind{"hit": {}, "leave": {}}, Actions: map[string]ActionSpec{"spawn": {Version: 1}}})
}

// 缺省：账本不淘汰，Limits.Effects 是实例一生里的动作总数，完成了的也占着。
func TestSettledActionsStayInTheLedgerByDefault(t *testing.T) {
	h := newHarness(t, ledgerProgram(t), nest.RollbackUndo, false, nil)
	must(t, h.run(initStart))
	for i := uint64(1); i <= 2; i++ {
		must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(i), int64(i)) }))
		completeAll(t, h, int64(i)) // 里面核对了同结论的重复回执是幂等的
	}
	if state := h.c.Snapshot(); len(state.Effects) != 2 || h.c.Project().Variables["global/done"] != Int(2) {
		t.Fatalf("ledger %+v", state.Effects)
	}
	if err := h.run(func(c *LevelComponent) error { return c.Handle(hit(3), 3) }); !errors.Is(err, ErrBudget) {
		t.Fatalf("third action with two settled ones in the ledger: %v, want ErrBudget", err)
	}
}

// 打开 ForgetSettledActions：有了回执或被取消的动作当次就离开账本，上限只管同时未完成的个数；
// ID 不复用，action.done 照常分派，晚到的重复回执是 ErrStale，出错的变更连同淘汰一起撤回。
func TestForgetSettledActionsKeepsOnlyPendingOnes(t *testing.T) {
	h := newHarness(t, ledgerProgram(t), nest.RollbackUndo, false, nil)
	h.c.ForgetSettledActions()
	must(t, h.run(initStart))
	seen := map[string]bool{}
	var last Receipt
	sequence := uint64(0)
	for i := 1; i <= 10; i++ {
		sequence++
		must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(sequence), int64(i)) }))
		pending := h.c.PendingActions()
		if len(pending) != 1 || seen[pending[0].ID] || pending[0].Sequence != uint64(i) {
			t.Fatalf("round %d: pending %+v (IDs so far %v)", i, pending, seen)
		}
		seen[pending[0].ID] = true
		last = Receipt{Instance: pending[0].Instance, Epoch: pending[0].Epoch, ID: pending[0].ID, Success: true}
		must(t, h.run(func(c *LevelComponent) error { return c.Complete(last, int64(i)) }))
		if state := h.c.Snapshot(); len(state.Effects) != 0 || state.NextEffect != uint64(i) || h.c.Project().Variables["global/done"] != Int(int64(i)) {
			t.Fatalf("round %d: ledger %+v next=%d view=%+v", i, state.Effects, state.NextEffect, h.c.Project().Variables)
		}
	}
	// 已经结算并淘汰的动作：重复回执不论结论都是作废，状态不动。
	before := h.c.Snapshot().Revision
	for _, success := range []bool{true, false} {
		duplicate := last
		duplicate.Success = success
		if err := h.run(func(c *LevelComponent) error { return c.Complete(duplicate, 11) }); !errors.Is(err, ErrStale) {
			t.Fatalf("duplicate receipt (success=%v) for a forgotten action: %v, want ErrStale", success, err)
		}
	}
	if h.c.Snapshot().Revision != before {
		t.Fatal("a stale receipt changed the state")
	}

	// 上限仍然管未完成的：两个 pending 之后第三个被拒，被拒的那次变更不留痕迹。
	for i := 0; i < 2; i++ {
		sequence++
		must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(sequence), 12) }))
	}
	if len(h.c.Snapshot().Effects) != 2 {
		t.Fatalf("pending actions were forgotten: %+v", h.c.Snapshot().Effects)
	}
	if err := h.run(func(c *LevelComponent) error { return c.Handle(hit(sequence+1), 12) }); !errors.Is(err, ErrBudget) {
		t.Fatalf("third pending action: %v, want ErrBudget", err)
	}
	pending := h.c.PendingActions()
	if len(pending) != 2 {
		t.Fatalf("pending after the rejected request: %+v", pending)
	}
	// 完成一个之后腾出一个位置。
	must(t, h.run(func(c *LevelComponent) error {
		return c.Complete(Receipt{Instance: pending[0].Instance, Epoch: pending[0].Epoch, ID: pending[0].ID}, 13)
	}))
	sequence++
	must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(sequence), 13) }))
	pending = h.c.PendingActions()
	if len(pending) != 2 || len(h.c.Snapshot().Effects) != 2 {
		t.Fatalf("after freeing one slot: %+v", h.c.Snapshot().Effects)
	}

	// 阶段结束取消它没完成的动作：被取消的也当次离开账本，晚到的回执作废。
	leave := hit(sequence + 1)
	leave.Kind = "leave"
	must(t, h.run(func(c *LevelComponent) error { return c.Handle(leave, 14) }))
	if state := h.c.Snapshot(); len(state.Effects) != 0 || len(h.c.PendingActions()) != 0 {
		t.Fatalf("cancelled actions stayed in the ledger: %+v", state.Effects)
	}
	late := Receipt{Instance: pending[0].Instance, Epoch: pending[0].Epoch, ID: pending[0].ID, Success: true}
	if err := h.run(func(c *LevelComponent) error { return c.Complete(late, 15) }); !errors.Is(err, ErrStale) {
		t.Fatalf("receipt for a cancelled, forgotten action: %v, want ErrStale", err)
	}
}

// 淘汰发生在事务里的草稿上：同一个 handler 后面出错，账本回到提交前（刚完成的动作还是 pending）。
func TestForgettingRollsBackWithTheTransaction(t *testing.T) {
	h := newHarness(t, ledgerProgram(t), nest.RollbackUndo, false, nil)
	h.c.ForgetSettledActions()
	must(t, h.run(initStart))
	must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(1), 1) }))
	action := h.c.PendingActions()[0]
	receipt := Receipt{Instance: action.Instance, Epoch: action.Epoch, ID: action.ID, Success: true}
	failure := errors.New("later step failed")
	if err := h.run(func(c *LevelComponent) error {
		if err := c.Complete(receipt, 2); err != nil {
			return err
		}
		if len(c.Snapshot().Effects) != 0 {
			t.Error("the settled action is still in the ledger inside the transaction")
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if pending := h.c.PendingActions(); len(pending) != 1 || pending[0].ID != action.ID || h.c.Project().Variables["global/done"] != Int(0) {
		t.Fatalf("after rollback: %+v", h.c.Snapshot().Effects)
	}
	must(t, h.run(func(c *LevelComponent) error { return c.Complete(receipt, 2) }))
	if len(h.c.Snapshot().Effects) != 0 || h.c.Project().Variables["global/done"] != Int(1) {
		t.Fatalf("retry: %+v", h.c.Snapshot().Effects)
	}
}
