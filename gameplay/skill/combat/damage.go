package combat

// DamageType 选择伤害管线里的抗性通道；其它取值不走抗性通道（真实伤害）。
type DamageType uint16

const (
	DamageTypeTrue     DamageType = 0
	DamageTypePhysical DamageType = 1 // 由 Armor 减免
	DamageTypeMagical  DamageType = 2 // 由 MagicResistance 减免
)

// Element 标识按元素区分伤害倍率的元素通道。
type Element uint16

// 战斗结果名：封闭词表，与 skill EventContext 的 result 字段及 proc 过滤一致。
const (
	ResultHit     = "hit"
	ResultImmune  = "immune"
	ResultDodged  = "dodged"
	ResultBlocked = "blocked"
	ResultParried = "parried"
	ResultKilled  = "killed"
)

// PipelineStages 记录固定的十二阶段伤害顺序。顺序是确定性契约的一部分，不随内容变化。
var PipelineStages = [12]string{
	"target_validity", "immunity", "avoidance", "damage_type",
	"penetration", "element", "modifiers", "critical",
	"caps", "shield", "health", "aftermath",
}

// Combatant 是管线读写的扁平定点属性块。Host 把自己的实体模型投影进来（或直接嵌入）；闪避与暴击是预先掷好的
// 布尔事实，管线本身不含随机性。
type Combatant struct {
	Alive     bool
	Health    int64
	MaxHealth int64
	Shield    int64

	Armor           int64
	MagicResistance int64
	Penetration     int64

	DamageDealtBP        int64 // 0 表示默认 10000
	DamageTakenBP        int64 // 0 表示默认 10000
	CriticalMultiplierBP int64 // 0 表示默认 15000
	// ElementMultipliersBP 按元素缩放伤害；缺项或 0 表示默认 10000，所以完全免疫某元素用 DamageImmune 标志或闪避事实
	// 表达，而不是 0 倍率。
	ElementMultipliersBP map[Element]int64
	VampBP               int64

	DamageCap     int64 // 0 表示不启用
	MinimumDamage int64 // 0 表示不启用

	Dodge         bool
	Parry         bool
	Block         bool
	DamageImmune  bool
	SpellShield   bool
	ForceCritical bool
}

// DamageInput 是进入管线的一次伤害实例。
type DamageInput struct {
	Amount      int64
	Type        DamageType
	Element     Element
	CanCritical bool
	// SpellTagged 把实例标为法术伤害，可以被法术护盾钩子拦截。
	SpellTagged bool
}

// DamageOutcome 报告管线做了什么。Result 是封闭结果词表里的值（或被消耗的钩子名），供事件上下文使用。
type DamageOutcome struct {
	Attempted    int64
	Mitigated    int64
	Absorbed     int64
	HealthDamage int64
	VampHeal     int64
	Dodged       bool
	Parried      bool
	Blocked      bool
	Critical     bool
	Immune       bool
	Killed       bool
	CombatHooks  []string
	Result       string
}

// Hooks 让嵌入方世界在管线固定的拦截点插入由状态驱动的战斗钩子。Peek 必须没有副作用；管线恰好在使用了被查看的
// 钩子时调用 ConsumeHook。Hooks 为 nil 时不做任何拦截。
type Hooks interface {
	// PeekSpellShield 报告对目标完全拦截一次法术伤害的钩子（“spell_shield”类状态）。
	PeekSpellShield() (string, bool)
	// PeekCriticalOverride 报告让来源必定暴击的钩子。
	PeekCriticalOverride() (string, bool)
	// PeekDeathPrevention 报告实例致命时把目标钉在 1 点生命的钩子（“death_prevention”或“execute_immunity”状态）。
	PeekDeathPrevention() (string, bool)
	// ConsumeHook 在管线用过钩子之后移除支撑它的状态。
	ConsumeHook(hook string)
	// OnShieldAbsorbed 把聚合护盾的吸收量同步到世界里各状态的护盾池；只在 absorbed > 0 时调用。
	OnShieldAbsorbed(absorbed int64)
}

// ResolveDamage 让一次伤害实例走完十二阶段管线，修改 target（生命、护盾、存活状态），吸血时还修改 source。source
// 可以为 nil（世界来源的伤害：没有穿透、没有强制暴击、没有吸血）。目标为 nil、已死亡或生命值被破坏成负数时返回
// ok=false 且没有副作用——如何报告无效目标由调用方决定。
//
// 计算是权威的“twelve_stage_v1”公式：抗性 R 按 10000/(10000+100R) 减免，比率以基点相乘组合，每一步都饱和而不回绕。
func ResolveDamage(source, target *Combatant, input DamageInput, hooks Hooks) (DamageOutcome, bool) {
	if target == nil || !target.Alive || target.Health < 0 {
		return DamageOutcome{}, false
	}
	outcome := DamageOutcome{Attempted: maxInt64(input.Amount, 0), Result: ResultHit}
	amount := outcome.Attempted

	if input.SpellTagged && hooks != nil {
		if hook, ok := hooks.PeekSpellShield(); ok {
			outcome.Immune = true
			outcome.CombatHooks = append(outcome.CombatHooks, hook)
			outcome.Result = hook
			hooks.ConsumeHook(hook)
			return outcome, true
		}
	}
	if target.DamageImmune || target.SpellShield {
		outcome.Immune = true
		outcome.Result = ResultImmune
		if target.SpellShield {
			target.SpellShield = false
		}
		return outcome, true
	}
	if target.Dodge {
		outcome.Dodged, amount, outcome.Result = true, 0, ResultDodged
		return outcome, true
	} else if target.Parry {
		outcome.Parried, amount, outcome.Result = true, 0, ResultParried
		return outcome, true
	} else if target.Block {
		outcome.Blocked, amount, outcome.Result = true, amount/2, ResultBlocked
	}

	penetration := int64(0)
	if source != nil {
		penetration = source.Penetration
	}
	resistance := int64(0)
	switch input.Type {
	case DamageTypePhysical:
		resistance = target.Armor
	case DamageTypeMagical:
		resistance = target.MagicResistance
	}
	resistance = maxInt64(0, saturatingInt64Sub(resistance, penetration))
	if resistance > 0 {
		denominator := saturatingInt64Add(BasisPointScale, saturatingInt64Mul(resistance, 100))
		amount = saturatingInt64Mul(amount, BasisPointScale) / denominator
	}
	outcome.Mitigated = amount

	// 比率阶段每次缩放后都夹到 0：Host 把修正层数投影进这些基点字段，层数把比率压到 -10000 以下时必须表示“无伤害”，
	// 而不是负伤害（那会变成加护盾或加血）。
	elementBP := target.ElementMultipliersBP[input.Element]
	if elementBP == 0 {
		elementBP = BasisPointScale
	}
	amount = maxInt64(ScaleBasisPoints(amount, elementBP), 0)
	dealtBP, takenBP := int64(0), target.DamageTakenBP
	if source != nil {
		dealtBP = source.DamageDealtBP
	}
	if dealtBP == 0 {
		dealtBP = BasisPointScale
	}
	if takenBP == 0 {
		takenBP = BasisPointScale
	}
	amount = maxInt64(ScaleBasisPoints(maxInt64(ScaleBasisPoints(amount, dealtBP), 0), takenBP), 0)

	criticalHook, criticalOverride := "", false
	if hooks != nil {
		criticalHook, criticalOverride = hooks.PeekCriticalOverride()
	}
	forceCritical := source != nil && source.ForceCritical
	if input.CanCritical && (forceCritical || criticalOverride) {
		criticalBP := int64(0)
		if source != nil {
			criticalBP = source.CriticalMultiplierBP
		}
		if criticalBP == 0 {
			criticalBP = 15000
		}
		amount = maxInt64(ScaleBasisPoints(amount, criticalBP), 0)
		outcome.Critical = true
		if criticalOverride {
			outcome.CombatHooks = append(outcome.CombatHooks, criticalHook)
			hooks.ConsumeHook(criticalHook)
		}
	}
	if target.DamageCap > 0 && amount > target.DamageCap {
		amount = target.DamageCap
	}
	if target.MinimumDamage > 0 && amount > 0 && amount < target.MinimumDamage {
		amount = target.MinimumDamage
	}

	// Host 写坏的负护盾不会放大伤害：吸收量只按护盾池的非负部分计算。
	outcome.Absorbed = minInt64(maxInt64(target.Shield, 0), amount)
	target.Shield -= outcome.Absorbed
	amount -= outcome.Absorbed
	if outcome.Absorbed > 0 && hooks != nil {
		hooks.OnShieldAbsorbed(outcome.Absorbed)
	}

	if amount >= target.Health && target.Health > 0 && hooks != nil {
		if hook, ok := hooks.PeekDeathPrevention(); ok {
			amount = maxInt64(0, target.Health-1)
			outcome.CombatHooks = append(outcome.CombatHooks, hook)
			outcome.Result = hook
			hooks.ConsumeHook(hook)
		}
	}
	outcome.HealthDamage = minInt64(target.Health, amount)
	target.Health -= outcome.HealthDamage
	if target.Health == 0 && outcome.HealthDamage > 0 {
		target.Alive = false
		outcome.Killed = true
		outcome.Result = ResultKilled
	}
	if source != nil && source.Alive && source.VampBP > 0 && outcome.HealthDamage > 0 {
		// 吸血遵守普通治疗的存活与缺血约束，结果只报告实际恢复量。
		heal, _ := ResolveHeal(source, ScaleBasisPoints(outcome.HealthDamage, source.VampBP))
		outcome.VampHeal = heal.Effective
	}
	return outcome, true
}

// HealOutcome 报告一次治疗的结算结果。
type HealOutcome struct {
	Attempted int64
	Effective int64
}

// ResolveHeal 施加以缺失生命为上限的治疗。目标为 nil 或已死亡（死亡目标不可治疗）时 ok=false。
func ResolveHeal(target *Combatant, amount int64) (HealOutcome, bool) {
	if target == nil || !target.Alive {
		return HealOutcome{}, false
	}
	attempted := maxInt64(amount, 0)
	effective := minInt64(attempted, maxInt64(0, saturatingInt64Sub(target.MaxHealth, target.Health)))
	target.Health += effective
	return HealOutcome{Attempted: attempted, Effective: effective}, true
}

// AddShield 饱和地增加护盾值，返回实际增加量。目标为 nil 或已死亡时 ok=false。
func AddShield(target *Combatant, amount int64) (int64, bool) {
	if target == nil || !target.Alive {
		return 0, false
	}
	attempted := maxInt64(amount, 0)
	before := target.Shield
	target.Shield = saturatingInt64Add(before, attempted)
	// 报告权威的增量而不是请求的数量：数值饱和时结果载荷与事件仍然一致。
	return target.Shield - before, true
}
