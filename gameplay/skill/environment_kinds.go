package skill

// Exported kind constants for catalog authoring. The underlying kind types
// are unexported, which previously made externally-built CompileEnvironment
// and GameplayCatalog entries impossible to construct with an explicit value
// type or quantity — e.g. a basis-points haste attribute driving a
// windup_ticks_expression. Constants of an unexported type are usable by any
// package even though the type itself cannot be named.

// Value kinds for catalog fields such as AttributeCatalogEntry.ValueType.
const (
	ValueKindInt    = valueKindInt
	ValueKindBool   = valueKindBool
	ValueKindString = valueKindString
)

// Quantity kinds for catalog fields such as AttributeCatalogEntry.Quantity
// and for IntRuntimeValue construction in Host implementations.
const (
	QuantityUnknown                 = quantityUnknown
	QuantityDimensionless           = quantityDimensionless
	QuantityCount                   = quantityCount
	QuantityTicks                   = quantityTicks
	QuantityWorldDistance           = quantityWorldDistance
	QuantityAngleMDeg               = quantityAngleMDeg
	QuantitySpeedWorldPerTick       = quantitySpeedWorldPerTick
	QuantityAngularSpeedMDegPerTick = quantityAngularSpeedMDegPerTick
	QuantityBasisPoints             = quantityBasisPoints
	QuantityCombatAmount            = quantityCombatAmount
	QuantityResourceAmount          = quantityResourceAmount
)

// AuthorityDigest computes the digest a CompileEnvironment must carry for
// its current catalog contents. A host that extends the default environment
// (for example adding a basis-points haste attribute) reseals it with this
// before compiling; without it, externally modified environments were
// rejected with ENVIRONMENT_INVALID and no recourse.
func AuthorityDigest(environment CompileEnvironment) string {
	return authorityDigest(environment)
}
