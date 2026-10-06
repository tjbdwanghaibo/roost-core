package combatcomponent

// skill O2（维护者第十二轮决定，2026-10-06：组件给投影入口，投影交业务）：伤害管线读 Combatant
// 的平铺字段，buff 与属性修饰只改 AttributeSet，以前组件没有把属性写到 Combatant 的入口
// （CombatDao.attributes 不导出，applyState 还会换掉 AttributeSet 实例），经 buff 加的护甲
// 对伤害没有任何影响。
//
// 承诺：业务用 ProjectAttributes 装上投影后，buff 改属性 → 同一事务里投影写进 vitals →
// 伤害随之改变；投影写的字段走 DAO 的回滚（A1），两种回滚策略 × handler 失败 / 提交被拒
// 都回到事务开始时的字节；实体从存储加载后（OnInitFinish）按当前属性重新投影，不产生持久写。

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/skill/combat"
)

const projectionArmor combat.AttributeID = 5

// projectArmor 是业务写的全部投影代码：属性 5 是护甲。
func projectArmor(attribute func(combat.AttributeID) int64, combatant *combat.Combatant) {
	combatant.Armor = attribute(projectionArmor)
}

func armorBuff(id combat.BuffID) combat.BuffSpec {
	return combat.BuffSpec{ID: id, DurationTicks: 50, Modifiers: []combat.Modifier{{Attribute: projectionArmor, Flat: 100}}}
}

func TestBuffAttributeModifierReachesDamage(t *testing.T) {
	getter := newTestGetter()
	defender, defenderID := newCombatTestEntity(t, 9301)
	defender.component.ProjectAttributes(projectArmor)
	getter.Add(defender)
	meta := nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict}
	var withoutBuff, withBuff, afterDispel combat.DamageOutcome
	handler := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		component := es[0].(*combatTestEntity).component
		component.InitCombatant(combat.Combatant{Alive: true, Health: 1000, MaxHealth: 1000})
		hit := func(outcome *combat.DamageOutcome) error {
			var ok bool
			if *outcome, ok = component.ApplyDamage(nil, combat.DamageInput{Amount: 100, Type: combat.DamageTypePhysical}, nil); !ok {
				return errors.New("damage did not land")
			}
			return nil
		}
		if err := hit(&withoutBuff); err != nil {
			return nil, err
		}
		buff, _ := component.ApplyBuff(armorBuff(11), 0, 0)
		if err := hit(&withBuff); err != nil {
			return nil, err
		}
		component.RemoveBuff(buff)
		return nil, hit(&afterDispel)
	}
	if _, err := runCombatHandler(t, getter, meta, acceptingCombatCommitter{}, "combat_projection", handler, defenderID); err != nil {
		t.Fatal(err)
	}
	if withoutBuff.HealthDamage != 100 || withBuff.HealthDamage != 50 || afterDispel.HealthDamage != 100 {
		t.Fatalf("health damage without / with / after removing the +100 armor buff = %d / %d / %d, want 100 / 50 / 100",
			withoutBuff.HealthDamage, withBuff.HealthDamage, afterDispel.HealthDamage)
	}
}

func TestAttributeProjectionRollsBackWithTheDao(t *testing.T) {
	failures := []struct {
		name      string
		committer nest.TransactionCommitter
		failAfter bool
	}{
		{name: "handler_fails", committer: acceptingCombatCommitter{}, failAfter: true},
		{name: "commit_rejected", committer: rejectingCombatCommitter{}},
	}
	uniqueID := int64(9310)
	for _, policy := range []nest.RollbackPolicy{nest.RollbackUndo, nest.RollbackState} {
		for _, failure := range failures {
			uniqueID++
			unique := uniqueID
			t.Run(policy.String()+"/"+failure.name, func(t *testing.T) {
				getter := newTestGetter()
				defender, defenderID := newCombatTestEntity(t, unique)
				defender.component.ProjectAttributes(projectArmor)
				getter.Add(defender)
				meta := nest.HandlerMeta{Rollback: policy, Durability: nest.DurabilityStrict}
				seed := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					component := es[0].(*combatTestEntity).component
					component.InitCombatant(combat.Combatant{Alive: true, Health: 1000, MaxHealth: 1000})
					component.SetAttributeBase(projectionArmor, 20)
					return nil, nil
				}
				if _, err := runCombatHandler(t, getter, meta, acceptingCombatCommitter{}, "combat_projection_seed", seed, defenderID); err != nil {
					t.Fatalf("seed: %v", err)
				}
				if got := defender.component.Combatant().Armor; got != 20 {
					t.Fatalf("armor after seeding base 20 = %d", got)
				}
				before := mustMarshal(t, defender.component.Dao())
				defender.component.Dao().CleanDirty()

				mutate := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
					component := es[0].(*combatTestEntity).component
					component.ApplyBuff(armorBuff(12), 0, 0)
					if got := component.Combatant().Armor; got != 120 {
						return nil, errors.New("buff did not project into armor")
					}
					if failure.failAfter {
						return nil, errors.New("boom")
					}
					return nil, nil
				}
				if _, err := runCombatHandler(t, getter, meta, failure.committer, "combat_projection_mutate", mutate, defenderID); err == nil {
					t.Fatal("the transaction was expected to fail")
				}
				if got := mustMarshal(t, defender.component.Dao()); !bytes.Equal(got, before) {
					t.Fatalf("combat state diverged after rollback:\n got %x\nwant %x", got, before)
				}
				if got := defender.component.Combatant().Armor; got != 20 {
					t.Fatalf("projected armor after rollback = %d, want 20", got)
				}
				if defender.component.Dao().DirtyTracker().HasSyncDirty() {
					t.Fatal("sync dirty masks survived rollback")
				}
			})
		}
	}
}

func TestAttributeProjectionReprojectsOnLoad(t *testing.T) {
	getter := newTestGetter()
	stored, storedID := newCombatTestEntity(t, 9320)
	getter.Add(stored)
	meta := nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict}
	// 存储里的 vitals 是没有投影（或旧投影）时提交的：属性护甲 50，Combatant.Armor 0。
	seed := func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		component := es[0].(*combatTestEntity).component
		component.InitCombatant(combat.Combatant{Alive: true, Health: 1000, MaxHealth: 1000})
		component.SetAttributeBase(projectionArmor, 50)
		return nil, nil
	}
	if _, err := runCombatHandler(t, getter, meta, acceptingCombatCommitter{}, "combat_projection_store", seed, storedID); err != nil {
		t.Fatal(err)
	}
	raw, schema, err := stored.component.Dao().MarshalPersisted()
	if err != nil {
		t.Fatal(err)
	}

	// 实体工厂的顺序：建 DAO 与组件、装投影，再从存储加载，最后 OnInitFinish。
	dao := NewCombatDao(storedID, "game")
	component := NewCombatComponent(dao)
	component.ProjectAttributes(projectArmor)
	if err := dao.RestorePersisted(raw, schema, 1); err != nil {
		t.Fatal(err)
	}
	if err := component.OnInitFinish(nil, false); err != nil {
		t.Fatal(err)
	}
	if got := component.Combatant().Armor; got != 50 {
		t.Fatalf("armor after load = %d, want the projected attribute 50", got)
	}
	if dao.DirtyTracker().HasSyncDirty() {
		t.Fatal("load-time projection marked the DAO dirty")
	}
}
