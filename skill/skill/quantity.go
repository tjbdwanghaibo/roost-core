package skill

type quantityKind uint8

const (
	quantityUnknown quantityKind = iota
	quantityDimensionless
	quantityCount
	quantityTicks
	quantityWorldDistance
	quantityAngleMDeg
	quantitySpeedWorldPerTick
	quantityAngularSpeedMDegPerTick
	quantityBasisPoints
	quantityCombatAmount
	quantityResourceAmount
)
