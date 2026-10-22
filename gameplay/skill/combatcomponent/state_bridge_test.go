package combatcomponent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/state"
)

// Buff → state（D21 S6，维护者决定 12：派生）：buff 标签经 BindStates 成为派生状态，查询时读容器，不另存；buff 的
// 每个改集合的 mutator 通知观察者；buff 随事务回滚，状态当场跟着对，观察者在下一次通知时跟上。

const (
	stateStunned state.ID = 1
	stateRooted  state.ID = 2
	stateSaved   state.ID = 3
)

func stateTestCatalog(t *testing.T) *state.Catalog {
	t.Helper()
	catalog, err := state.NewCatalog(
		state.Def{ID: stateStunned, Name: "stunned", Sync: true},
		state.Def{ID: stateRooted, Name: "rooted"},
		state.Def{ID: stateSaved, Name: "saved", Persist: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestBuffTagsDriveDerivedStates(t *testing.T) {
	nest.ResetHandlersForTest()
	t.Cleanup(nest.ResetHandlersForTest)
	getter := newTestGetter()
	target, targetID := newCombatTestEntity(t, 9101)
	getter.Add(target)
	set := state.NewSet(state.NewStateDao(targetID, "game", stateTestCatalog(t)), state.SetConfig{OnError: func(err error) { t.Errorf("state: %v", err) }})
	if err := target.component.BindStates(set,
		StateBinding{State: stateStunned, Tags: []combat.Tag{"stun", "freeze"}},
		StateBinding{State: stateRooted, Tags: []combat.Tag{"root"}},
	); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, id := range []state.ID{stateStunned, stateRooted} {
		if _, err := set.Watch(id, func(c state.Change) { seen = append(seen, fmt.Sprintf("%d %v", c.ID, c.Active)) }); err != nil {
			t.Fatal(err)
		}
	}
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithTransactionCommitter(acceptingCombatCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}),
		nest.NestOptionWithTickDuration(100*time.Millisecond),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()
	name := nest.NewHandlerName("combat_state_run")
	nest.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		return nil, params[0].(func() error)()
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	run := func(fn func() error) error {
		_, err := engine.Request(context.Background(), name, targetID, nest.NewParams(fn))
		return err
	}
	c := target.component
	var stun combat.BuffInstanceID
	if err := run(func() error {
		stun, _ = c.ApplyBuff(combat.BuffSpec{ID: 1, Tags: []combat.Tag{"stun"}, DurationTicks: 10}, 0, 0)
		if !set.Has(stateStunned) || set.Has(stateRooted) || !c.HasBuffTag("stun") {
			return errors.New("stun buff applied but the state does not hold")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(set.Sources(stateStunned)) != 0 {
		t.Fatal("the derived state was copied into the state DAO")
	}
	// 回滚：buff 撤销，派生状态当场跟着对；观察者此时还没收到“rooted false”，下一次通知补上。
	errRefused := errors.New("refused")
	if err := run(func() error {
		c.ApplyBuff(combat.BuffSpec{ID: 2, Tags: []combat.Tag{"root"}}, 1, 0)
		return errRefused
	}); !errors.Is(err, errRefused) {
		t.Fatal(err)
	}
	if set.Has(stateRooted) {
		t.Fatal("a rolled-back root buff still roots")
	}
	if err := run(func() error {
		expired := c.TickBuffs(20)
		if len(expired) != 1 || expired[0].Instance != stun {
			return fmt.Errorf("expired %+v", expired)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := run(func() error {
		c.ApplyBuff(combat.BuffSpec{ID: 3, Tags: []combat.Tag{"freeze"}}, 21, 0)
		if removed := c.DispelBuffs("freeze", 0); len(removed) != 1 {
			return fmt.Errorf("dispelled %+v", removed)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := "[1 true 2 true 1 false 2 false 1 true 1 false]"
	if fmt.Sprint(seen) != want {
		t.Fatalf("watchers saw %v, want %v", seen, want)
	}
	if !set.Dao().DirtyTracker().HasSyncDirty() {
		t.Fatal("a synced derived state changed without marking the sync bit")
	}
}

// 绑定的校验：每个状态一个提供函数，持久状态不能派生，标签不能为空。
func TestBindStatesRefusesBadBindings(t *testing.T) {
	target, targetID := newCombatTestEntity(t, 9102)
	set := state.NewSet(state.NewStateDao(targetID, "game", stateTestCatalog(t)), state.SetConfig{})
	if err := target.component.BindStates(set, StateBinding{State: stateStunned, Tags: []combat.Tag{"stun"}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []StateBinding{
		{State: stateStunned, Tags: []combat.Tag{"freeze"}},
		{State: stateSaved, Tags: []combat.Tag{"shield"}},
		{State: stateRooted},
		{State: 99, Tags: []combat.Tag{"x"}},
	} {
		if err := target.component.BindStates(set, bad); err == nil {
			t.Errorf("BindStates(%+v) accepted", bad)
		}
	}
	if err := target.component.BindStates(nil); err == nil {
		t.Error("BindStates without a set accepted")
	}
}

// 加载 / 新建完成（OnInitFinish）时通知一次：装门控、登记观察者早于从存储恢复 buff 时，观察者在这里跟上已经生效的
// 控制效果。
func TestLoadedBuffsReachBoundStatesOnInitFinish(t *testing.T) {
	target, targetID := newCombatTestEntity(t, 9103)
	set := state.NewSet(state.NewStateDao(targetID, "game", stateTestCatalog(t)), state.SetConfig{})
	if err := target.component.BindStates(set, StateBinding{State: stateStunned, Tags: []combat.Tag{"stun"}}); err != nil {
		t.Fatal(err)
	}
	var seen []bool
	if _, err := set.Watch(stateStunned, func(c state.Change) { seen = append(seen, c.Active) }); err != nil {
		t.Fatal(err)
	}
	// 相当于 RestorePersisted 恢复出一个 stun buff（不经组件的 mutator）。
	target.component.Dao().buffs.Apply(combat.BuffSpec{ID: 1, Tags: []combat.Tag{"stun"}}, 0, 0)
	if err := target.component.OnInitFinish(nil, false); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != "[true]" {
		t.Fatalf("watcher saw %v after OnInitFinish, want [true]", seen)
	}
}

// 每个改 buff 集合的 mutator 都在同一事务里通知绑定的派生状态（BindStates 的契约）：派生值查询时总是对的，观察者
// （门控、AI）只能靠这次通知跟上，漏掉的 mutator 会让门控停在旧值，直到下一次别的改动。逐个 mutator 断言观察者
// 当场看到变化：RemoveBuff、SetBuffStacks（改层数不变集合、改成 0 移除）、AdoptBuff。
func TestEveryBuffMutatorNotifiesBoundStates(t *testing.T) {
	nest.ResetHandlersForTest()
	t.Cleanup(nest.ResetHandlersForTest)
	getter := newTestGetter()
	target, targetID := newCombatTestEntity(t, 9104)
	getter.Add(target)
	set := state.NewSet(state.NewStateDao(targetID, "game", stateTestCatalog(t)), state.SetConfig{OnError: func(err error) { t.Errorf("state: %v", err) }})
	if err := target.component.BindStates(set, StateBinding{State: stateStunned, Tags: []combat.Tag{"stun"}}); err != nil {
		t.Fatal(err)
	}
	var seen []bool
	if _, err := set.Watch(stateStunned, func(c state.Change) { seen = append(seen, c.Active) }); err != nil {
		t.Fatal(err)
	}
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithTransactionCommitter(acceptingCombatCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}),
		nest.NestOptionWithTickDuration(100*time.Millisecond),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()
	name := nest.NewHandlerName("combat_state_mutators")
	nest.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		return nil, params[0].(func() error)()
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	c := target.component
	step := func(what string, fn func() error, want string) {
		t.Helper()
		seen = nil
		if _, err := engine.Request(context.Background(), name, targetID, nest.NewParams(fn)); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if fmt.Sprint(seen) != want {
			t.Fatalf("%s: watcher saw %v, want %s", what, seen, want)
		}
	}
	var stun combat.BuffInstanceID
	step("apply", func() error {
		stun, _ = c.ApplyBuff(combat.BuffSpec{ID: 1, Tags: []combat.Tag{"stun"}, MaxStacks: 5}, 0, 0)
		return nil
	}, "[true]")
	var removed combat.BuffInstance
	step("remove", func() error {
		var ok bool
		if removed, ok = c.RemoveBuff(stun); !ok {
			return errors.New("stun not removed")
		}
		return nil
	}, "[false]")
	step("apply again", func() error {
		stun, _ = c.ApplyBuff(combat.BuffSpec{ID: 1, Tags: []combat.Tag{"stun"}, MaxStacks: 5}, 1, 0)
		return nil
	}, "[true]")
	step("set stacks to 0", func() error {
		if _, ok := c.SetBuffStacks(stun, 0); !ok {
			return errors.New("stacks not set")
		}
		return nil
	}, "[false]")
	step("adopt", func() error {
		stun = c.AdoptBuff(removed) // 例如从另一个实体转移来的实例
		return nil
	}, "[true]")
	step("set stacks to 3", func() error {
		if _, ok := c.SetBuffStacks(stun, 3); !ok {
			return errors.New("stacks not set")
		}
		return nil
	}, "[]")
}
