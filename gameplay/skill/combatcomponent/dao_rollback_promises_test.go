package combatcomponent

// A1（维护者 2026-10-05：回滚统一走 DAO，docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md）：战斗状态的回滚
// 由 CombatDao 自己负责——undo 策略下 DAO 的改动方法登记逆操作，state 策略下 DAO 快照恢复——组件只调 DAO，不登记 undo。
// 承诺：经真实 Nest 派发，两种策略 × handler 失败 / 提交被拒，伤害（双方 vitals）、属性、驱散与加 buff 全部回到事务开始时
// 的字节，buff 的属性加成随之恢复，同步脏位不残留。

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

type acceptingCombatCommitter struct{}

func (acceptingCombatCommitter) Commit(context.Context, nest.CommitRecord) error { return nil }

type rejectingCombatCommitter struct{}

func (rejectingCombatCommitter) Commit(context.Context, nest.CommitRecord) error {
	return errors.New("admission refused")
}

func TestCombatRollbackIsTheDaoRollback(t *testing.T) {
	policies := []nest.RollbackPolicy{nest.RollbackUndo, nest.RollbackState}
	failures := []struct {
		name      string
		committer nest.TransactionCommitter
		failAfter bool
	}{
		{name: "handler_fails", committer: acceptingCombatCommitter{}, failAfter: true},
		{name: "commit_rejected", committer: rejectingCombatCommitter{}},
	}
	uniqueID := int64(9100)
	for _, policy := range policies {
		for _, failure := range failures {
			uniqueID += 2
			defenderUnique, attackerUnique := uniqueID, uniqueID+1
			t.Run(policy.String()+"/"+failure.name, func(t *testing.T) {
				getter := newTestGetter()
				attacker, attackerID := newCombatTestEntity(t, attackerUnique)
				defender, defenderID := newCombatTestEntity(t, defenderUnique)
				getter.Add(attacker)
				getter.Add(defender)
				meta := nest.HandlerMeta{Rollback: policy, Durability: nest.DurabilityStrict}

				seed := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					defenderEntity := es[0].(*combatTestEntity)
					attackerEntity := es[1].(*combatTestEntity)
					defenderEntity.component.InitCombatant(combat.Combatant{Alive: true, Health: 100, MaxHealth: 100, Armor: 30})
					defenderEntity.component.SetAttributeBase(1, 100)
					defenderEntity.component.ApplyBuff(combat.BuffSpec{ID: 7, Tags: []combat.Tag{"magic"}, DurationTicks: 50, Modifiers: []combat.Modifier{{Attribute: 1, Flat: 25}}}, 0, attackerID)
					attackerEntity.component.InitCombatant(combat.Combatant{Alive: true, Health: 40, MaxHealth: 80, VampBP: 5000})
					return nil, nil
				}
				if _, err := runCombatHandler(t, getter, meta, acceptingCombatCommitter{}, "combat_seed", seed, defenderID, attackerID); err != nil {
					t.Fatalf("seed: %v", err)
				}
				defenderBefore := mustMarshal(t, defender.component.Dao())
				attackerBefore := mustMarshal(t, attacker.component.Dao())
				defender.component.Dao().CleanDirty()
				attacker.component.Dao().CleanDirty()

				mutate := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					defenderEntity := es[0].(*combatTestEntity)
					attackerEntity := es[1].(*combatTestEntity)
					outcome, ok := defenderEntity.component.ApplyDamage(attackerEntity.component, combat.DamageInput{Amount: 40, Type: combat.DamageTypePhysical}, nil)
					if !ok || outcome.HealthDamage == 0 || outcome.VampHeal == 0 {
						return nil, errors.New("damage did not land")
					}
					// 迁移/复制的目的实体与吸血来源都在 RequestMulti 的声明目标中。
					attackerEntity.component.AdoptBuff(defenderEntity.component.ActiveBuffs()[0])
					defenderEntity.component.SetAttributeBase(1, 5)
					defenderEntity.component.DispelBuffs("magic", 0)
					defenderEntity.component.ApplyBuff(combat.BuffSpec{ID: 8, DurationTicks: 10}, 1, 0)
					if failure.failAfter {
						return nil, errors.New("boom")
					}
					return nil, nil
				}
				if _, err := runCombatHandler(t, getter, meta, failure.committer, "combat_mutate", mutate, defenderID, attackerID); err == nil {
					t.Fatal("the transaction was expected to fail")
				}
				if got := mustMarshal(t, defender.component.Dao()); !bytes.Equal(got, defenderBefore) {
					t.Fatalf("defender state diverged after rollback:\n got %x\nwant %x", got, defenderBefore)
				}
				if got := mustMarshal(t, attacker.component.Dao()); !bytes.Equal(got, attackerBefore) {
					t.Fatal("attacker state diverged after rollback (the lifesteal heal survived)")
				}
				if defender.component.Dao().DirtyTracker().HasSyncDirty() || attacker.component.Dao().DirtyTracker().HasSyncDirty() {
					t.Fatal("sync dirty masks survived rollback")
				}
				if got := defender.component.AttributeCurrent(1); got != 125 {
					t.Fatalf("attribute current = %d after rollback, want 125 (base 100 + the restored buff's 25)", got)
				}
			})
		}
	}
}

func runCombatHandler(t *testing.T, getter *testGetter, meta nest.HandlerMeta, committer nest.TransactionCommitter, name string, handler nest.BaseHandler, ids ...int64) (any, error) {
	t.Helper()
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithTransactionCommitter(committer),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}),
	)
	handlerName := nest.NewHandlerName(name)
	if err := engine.RegisterHandlerWithMeta(handlerName, handler, meta); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()
	return engine.RequestMulti(context.Background(), handlerName, ids, nil)
}
