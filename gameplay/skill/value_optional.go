package skill

type valueKind uint8

const (
	valueKindInvalid valueKind = iota
	valueKindNull
	valueKindInt
	valueKindBool
	valueKindString
	valueKindEntity
	valueKindPosition
	valueKindHit
	valueKindPath
	valueKindDirection
	valueKindSpawn
	valueKindAttribute
	valueKindElement
	valueKindGameplayTag
	valueKindAbility
	valueKindStatusInstance
	valueKindSnapshotToken
	valueKindEntityList
	valueKindStringList
	valueKindEffectResult
)

type valueType struct {
	Base                valueKind
	Optional            bool
	Quantity            quantityKind
	Result              resultType
	Outcome             resultOutcomeScope
	ResultValueBase     valueKind
	ResultValueQuantity quantityKind
	ResultValueOptional bool
}
