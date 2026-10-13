package skill

// 编写 catalog 用的导出种类常量。底层种类类型不导出，外部构造 CompileEnvironment 与 GameplayCatalog 条目时
// 写不出显式的值类型或量纲（例如驱动 windup_ticks_expression 的基点急速属性）；未导出类型的常量在任何包里都能
// 使用，即使类型本身写不出名字。

// 值种类，用于 AttributeCatalogEntry.ValueType 等 catalog 字段。
const (
	ValueKindInt    = valueKindInt
	ValueKindBool   = valueKindBool
	ValueKindString = valueKindString
)

// 量纲种类，用于 AttributeCatalogEntry.Quantity 等 catalog 字段，以及 Host 实现里构造 IntRuntimeValue。
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

// AuthorityDigest 计算 CompileEnvironment 按当前 catalog 内容必须携带的 digest。扩展默认环境的 Host（例如加一个
// 基点急速属性）在编译前用它重新封装环境，否则修改过的环境会以 ENVIRONMENT_INVALID 被拒绝。
func AuthorityDigest(environment CompileEnvironment) string {
	return authorityDigest(environment)
}
