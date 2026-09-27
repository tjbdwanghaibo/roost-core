package combatcomponent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/skill"
	"github.com/tjbdwanghaibo/roost-core/skill/combat"
)

// OPEN-ITEMS B04：HostAdapter.Apply 在 nest.CurrentRollbackTx()==nil 时自开 RunDetachedTransaction。memory handler（RollbackNone +
// DurabilityMemory）没有 RollbackTx，正好满足这个条件，所以 RR-20260926-76 的“handler 内独立事务结果未知 → 返回前 fence 引擎”
// 覆盖到它：committer 返回 ErrCommitIndeterminate、业务吞掉错误时，FenceError 已满足 ErrNestFenced 与 ErrCommitIndeterminate，
// 后续请求被拒。这是影响面核实，不是缺陷：RR-76 修复已经生效。

type combatIndeterminateCommitter struct{ calls atomic.Int64 }

func (c *combatIndeterminateCommitter) Commit(context.Context, nest.CommitRecord) error {
	c.calls.Add(1)
	return fmt.Errorf("%w: simulated WAL outcome unknown", nest.ErrCommitIndeterminate)
}

func TestHostAdapterApplyInMemoryHandlerFencesOnIndeterminateOutcome(t *testing.T) {
	getter := newTestGetter()
	attacker, attackerID := newCombatTestEntity(t, 9101)
	defender, defenderID := newCombatTestEntity(t, 9102)
	getter.Add(attacker)
	getter.Add(defender)
	attacker.component.dao.combatant = combat.Combatant{Alive: true, Health: 80, MaxHealth: 80}
	defender.component.dao.combatant = combat.Combatant{Alive: true, Health: 100, MaxHealth: 100}
	committer := &combatIndeterminateCommitter{}
	adapter := &HostAdapter{
		Resolver:  mapResolver{1: attacker.component, 2: defender.component},
		Revision:  &testRevision{},
		Committer: committer,
	}
	engine := nest.NewEngine(nest.NestOptionWithGetter(getter), nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := nest.NewHandlerName("b04_combat_apply_in_memory_handler")
	var attempts atomic.Int64
	var applyErr, fencedInHandler error
	var hadTx bool
	engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
		attempts.Add(1)
		hadTx = nest.CurrentRollbackTx() != nil
		_, _, applyErr = adapter.Apply(skill.EffectCommand{Payload: skill.DamageCommand{Source: 1, Target: 2, Amount: 10}})
		fencedInHandler = engine.FenceError() // 业务吞掉结果未知
		return "ok", nil
	}, nest.HandlerMeta{})
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := engine.RequestMulti(ctx, name, []int64{defenderID, attackerID}, nil); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if hadTx || committer.calls.Load() != 1 || !errors.Is(applyErr, nest.ErrCommitIndeterminate) {
		t.Fatalf("premise: memory handler had RollbackTx=%v, detached commits=%d, Apply err=%v; want no tx, one indeterminate detached commit", hadTx, committer.calls.Load(), applyErr)
	}
	if !errors.Is(fencedInHandler, nest.ErrNestFenced) || !errors.Is(fencedInHandler, nest.ErrCommitIndeterminate) {
		t.Fatalf("FenceError=%v when HostAdapter.Apply returned an indeterminate outcome inside a memory handler: want ErrNestFenced + ErrCommitIndeterminate (RR-76)", fencedInHandler)
	}
	if _, err := engine.RequestMulti(ctx, name, []int64{defenderID, attackerID}, nil); !errors.Is(err, nest.ErrNestFenced) || attempts.Load() != 1 {
		t.Fatalf("second request err=%v attempts=%d: a fenced engine must refuse new work", err, attempts.Load())
	}
}
