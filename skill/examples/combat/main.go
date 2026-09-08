package main

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-skill/combat"
)

func main() {
	const attrArmor = combat.AttributeID(1)

	// 属性集：base + 修饰器，结果与授予顺序无关。
	attributes := combat.NewAttributeSet()
	attributes.SetBase(attrArmor, 30)

	// Combatant 是伤害管线读写的平铺定点数值块；用 Observe 把属性投影进去。
	target := &combat.Combatant{Alive: true, Health: 500, MaxHealth: 500}
	attributes.Observe(func(id combat.AttributeID) {
		if id == attrArmor {
			target.Armor = attributes.Current(attrArmor)
		}
	})

	// Buff 容器：叠层/驱散/免疫/韧性，LinkAttributes 让 buff 自动物化为属性修饰。
	buffs := combat.NewBuffContainer()
	buffs.LinkAttributes(attributes)
	buffs.Apply(combat.BuffSpec{
		ID: 7, Tags: []combat.Tag{"defense"}, DurationTicks: 100,
		Modifiers: []combat.Modifier{{Attribute: attrArmor, Flat: 20, RateBP: 1000}}, // +20 且 +10%
	}, 0, 99)

	// 一次伤害解析：随机性外置，闪避/暴击作为预掷事实传入（这里都为 false）。
	source := &combat.Combatant{Alive: true, Health: 400, MaxHealth: 400}
	outcome, ok := combat.ResolveDamage(source, target,
		combat.DamageInput{Amount: 200, Type: combat.DamageTypePhysical, CanCritical: true}, nil)

	fmt.Printf("ok=%v armor=%d result=%s dealt=%d targetHP=%d\n",
		ok, target.Armor, outcome.Result, outcome.HealthDamage, target.Health)
}
