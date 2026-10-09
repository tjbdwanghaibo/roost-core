package skill

type combatStage uint8

const (
	combatTargetValidity combatStage = iota + 1
	combatImmunity
	combatAvoidance
	combatDamageType
	combatPenetration
	combatElement
	combatModifiers
	combatCritical
	combatCaps
	combatShield
	combatHealth
	combatAftermath
)

var fixedCombatPipeline = [...]combatStage{
	combatTargetValidity, combatImmunity, combatAvoidance, combatDamageType,
	combatPenetration, combatElement, combatModifiers, combatCritical,
	combatCaps, combatShield, combatHealth, combatAftermath,
}
