package combat

// DamageType selects the resistance channel in the damage pipeline. Any
// other value takes no resistance channel (true damage).
type DamageType uint16

const (
	DamageTypeTrue     DamageType = 0
	DamageTypePhysical DamageType = 1 // mitigated by Armor
	DamageTypeMagical  DamageType = 2 // mitigated by MagicResistance
)

// Element identifies an element channel for per-element damage multipliers.
type Element uint16

// Combat result names, a closed vocabulary matching the skill EventContext
// result field and proc filters.
const (
	ResultHit     = "hit"
	ResultImmune  = "immune"
	ResultDodged  = "dodged"
	ResultBlocked = "blocked"
	ResultParried = "parried"
	ResultKilled  = "killed"
)

// PipelineStages documents the fixed twelve-stage damage order. The order is
// part of the determinism contract and never varies by content.
var PipelineStages = [12]string{
	"target_validity", "immunity", "avoidance", "damage_type",
	"penetration", "element", "modifiers", "critical",
	"caps", "shield", "health", "aftermath",
}

// Combatant is the flat fixed-point stat block the pipeline reads and
// writes. Hosts project their entity model into it (or embed it directly);
// avoidance and critical facts are pre-rolled booleans so the pipeline stays
// free of randomness.
type Combatant struct {
	Alive     bool
	Health    int64
	MaxHealth int64
	Shield    int64

	Armor           int64
	MagicResistance int64
	Penetration     int64

	DamageDealtBP        int64 // 0 means the 10000 default
	DamageTakenBP        int64 // 0 means the 10000 default
	CriticalMultiplierBP int64 // 0 means the 15000 default
	// ElementMultipliersBP scales damage per element; a missing or zero entry
	// means the 10000 default, so full element immunity is expressed with the
	// DamageImmune flag or a dodge fact, never a 0 multiplier.
	ElementMultipliersBP map[Element]int64
	VampBP               int64

	DamageCap     int64 // 0 disables
	MinimumDamage int64 // 0 disables

	Dodge         bool
	Parry         bool
	Block         bool
	DamageImmune  bool
	SpellShield   bool
	ForceCritical bool
}

// DamageInput is one damage instance entering the pipeline.
type DamageInput struct {
	Amount      int64
	Type        DamageType
	Element     Element
	CanCritical bool
	// SpellTagged marks the instance as spell damage, making it interceptable
	// by a spell-shield hook.
	SpellTagged bool
}

// DamageOutcome reports what the pipeline did. Result carries the closed
// result vocabulary (or a consumed hook name) for event contexts.
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

// Hooks lets the embedding world interpose status-driven combat hooks at the
// pipeline's fixed interception points. Peek must be side-effect free; the
// pipeline calls ConsumeHook exactly when a peeked hook is used. A nil Hooks
// disables all interceptions.
type Hooks interface {
	// PeekSpellShield reports the hook that fully intercepts a spell-tagged
	// damage instance against the target ("spell_shield"-category statuses).
	PeekSpellShield() (string, bool)
	// PeekCriticalOverride reports the hook that forces a critical hit for
	// the source.
	PeekCriticalOverride() (string, bool)
	// PeekDeathPrevention reports the hook that pins the target at 1 health
	// when the instance would be lethal ("death_prevention" or
	// "execute_immunity" statuses).
	PeekDeathPrevention() (string, bool)
	// ConsumeHook removes the hook's backing status after the pipeline used it.
	ConsumeHook(hook string)
	// OnShieldAbsorbed mirrors aggregate-shield absorption into the world's
	// per-status shield pools. Called only when absorbed > 0.
	OnShieldAbsorbed(absorbed int64)
}

// ResolveDamage runs one damage instance through the twelve-stage pipeline,
// mutating target (health, shield, life state) and, for vamp, source. The
// source may be nil (world-sourced damage: no penetration, no crit force, no
// vamp). It returns ok=false without side effects when the target is nil or
// already dead — the caller decides how to report an invalid target.
//
// The math is the authoritative "twelve_stage_v1" formula: resistance R
// mitigates by 10000/(10000+100R), rates compose multiplicatively in basis
// points, and every step saturates instead of wrapping.
func ResolveDamage(source, target *Combatant, input DamageInput, hooks Hooks) (DamageOutcome, bool) {
	if target == nil || !target.Alive {
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
	} else if target.Parry {
		outcome.Parried, amount, outcome.Result = true, 0, ResultParried
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

	// Rate stages clamp to zero after each scale: hosts project modifier
	// stacks into these BP fields, and a stack driven below -10000 must mean
	// "no damage", never negative damage (which would grant shield or health).
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

	// A host-corrupted negative shield never amplifies damage: absorption is
	// computed against the non-negative part of the pool.
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
	if source != nil && source.VampBP > 0 && outcome.HealthDamage > 0 {
		outcome.VampHeal = ScaleBasisPoints(outcome.HealthDamage, source.VampBP)
		source.Health = minInt64(source.MaxHealth, saturatingInt64Add(source.Health, outcome.VampHeal))
	}
	return outcome, true
}

// HealOutcome reports a resolved heal.
type HealOutcome struct {
	Attempted int64
	Effective int64
}

// ResolveHeal applies a heal capped at missing health. ok=false when the
// target is nil or dead (dead targets are not healable).
func ResolveHeal(target *Combatant, amount int64) (HealOutcome, bool) {
	if target == nil || !target.Alive {
		return HealOutcome{}, false
	}
	attempted := maxInt64(amount, 0)
	effective := minInt64(attempted, maxInt64(0, target.MaxHealth-target.Health))
	target.Health += effective
	return HealOutcome{Attempted: attempted, Effective: effective}, true
}

// AddShield grants shield points, saturating, and returns the amount added.
// ok=false when the target is nil or dead.
func AddShield(target *Combatant, amount int64) (int64, bool) {
	if target == nil || !target.Alive {
		return 0, false
	}
	attempted := maxInt64(amount, 0)
	before := target.Shield
	target.Shield = saturatingInt64Add(before, attempted)
	// Report the authoritative delta, not the requested amount. This keeps
	// result payloads and events consistent when the value saturates.
	return target.Shield - before, true
}
