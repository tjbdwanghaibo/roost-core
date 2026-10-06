// StatusBridge、属性投影与确定性掷点演示：skill 的 status / attribute-modifier
// 效果命令落到 combat 容器，业务的属性投影把护甲修饰写进伤害管线读的 Combatant，
// 暴击/闪避作为 HMAC 掷点事实进入伤害管线。
// 运行：go run ./statusbridge
package main

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/skill"
	"github.com/tjbdwanghaibo/roost-core/skill/combat"
	"github.com/tjbdwanghaibo/roost-core/skill/combatcomponent"
)

const (
	attrArmor combat.AttributeID = 1
	attrHaste combat.AttributeID = 3
)

type resolver map[skill.EntityID]*combatcomponent.CombatComponent

func (r resolver) CombatComponent(id skill.EntityID) (*combatcomponent.CombatComponent, bool) {
	component, ok := r[id]
	return component, ok
}

type world struct {
	revision skill.WorldRevision
}

func (w *world) CurrentRevision() skill.WorldRevision { return w.revision }
func (w *world) CommitEffect(events []combatcomponent.EffectEvent) skill.CommitReceipt {
	w.revision++
	for _, event := range events {
		fmt.Printf("event: %-24s entity=%d result=%s\n", event.Kind, event.Entity, event.Context.Result)
	}
	return skill.CommitReceipt{Revision: w.revision}
}

// projectCombat 是业务写的全部投影代码：哪个属性写到伤害管线的哪个字段。组件在每次
// 改属性 base 或 buff 的同一事务里调用它，结果随 DAO 提交、随 DAO 回滚。
func projectCombat(attribute func(combat.AttributeID) int64, combatant *combat.Combatant) {
	combatant.Armor = attribute(attrArmor)
}

// acceptAll 代替生产环境的 WAL / DataEngine 提交器。
type acceptAll struct{}

func (acceptAll) Commit(context.Context, nest.CommitRecord) error { return nil }

func main() {
	// 战斗组件的改动必须在 Nest 事务里（DAO 登记逆操作）；生产环境在 nest handler 里，
	// 这里用一个独立事务包住整段演示。
	if _, err := nest.RunDetachedTransaction(context.Background(), acceptAll{}, "statusbridge_demo", func() (any, error) {
		demo()
		return nil, nil
	}); err != nil {
		panic(err)
	}
}

func demo() {
	// 两个战斗组件（生产环境由 roost-core 实体工厂持有；这里独立使用）。
	attacker := combatcomponent.NewCombatComponent(combatcomponent.NewCombatDao(1, "game"))
	attacker.InitCombatant(combat.Combatant{Alive: true, Health: 300, MaxHealth: 300, CriticalMultiplierBP: 20000})
	defender := combatcomponent.NewCombatComponent(combatcomponent.NewCombatDao(2, "game"))
	// 属性 → Combatant 投影：生产环境在实体工厂构造组件后装一次。
	defender.ProjectAttributes(projectCombat)
	defender.InitCombatant(combat.Combatant{Alive: true, Health: 500, MaxHealth: 500})
	defender.SetAttributeBase(attrArmor, 40)

	tick := skill.Tick(0)
	bridge := &combatcomponent.StatusBridge{
		Resolver: resolver{1: attacker, 2: defender},
		Revision: &world{},
		Catalog: skill.GameplayCatalog{Statuses: skill.StatusCatalog{Entries: []skill.StatusCatalogEntry{
			{Handle: 20, Key: "sunder", Category: "debuff", DispelCategory: "physical", Dispellable: true, MaxStacks: 3,
				AttributeModifiers: []skill.StatusAttributeModifier{{Attribute: skill.AttributeHandle(attrArmor), Operation: "mul_bp", Value: 7500}}}, // -25% 护甲/层
		}}},
		CurrentTick: func() skill.Tick { return tick },
	}

	// 破甲两层：40 × (1 - 0.25×2) = 20。
	bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{
		SourceOwner: 1, Target: 2, Status: 20, DurationTicks: 60, Stacks: 2,
	}})
	fmt.Println("defender armor after sunder:", defender.Combatant().Armor)

	// 确定性掷点：同一坐标永远同一结果，副本/回放位一致。
	// 推荐坐标：RootEventID、EventID、EffectIndex、目标实体。
	matchSeed := []byte("match-7391-seed")
	crit := combat.ChanceRoll(matchSeed, "crit", 3000 /* 30% */, 1001 /* root event */, 2 /* target */)
	fmt.Println("crit roll (30%):", crit)

	source := attacker.Combatant()
	source.ForceCritical = crit
	attacker.InitCombatant(source)
	outcome, _ := defender.ApplyDamage(attacker, combat.DamageInput{Amount: 120, Type: combat.DamageTypePhysical, CanCritical: true}, nil)
	fmt.Printf("damage: attempted=%d dealt=%d critical=%v defenderHP=%d\n",
		outcome.Attempted, outcome.HealthDamage, outcome.Critical, defender.Combatant().Health)

	// 驱散按类别、newest-first；属性修饰即时回滚。
	bridge.Apply(skill.EffectCommand{Payload: skill.DispelStatusCommand{Target: 2, Category: "physical", Count: 0}})
	fmt.Println("defender armor after dispel:", defender.Combatant().Armor)
}
