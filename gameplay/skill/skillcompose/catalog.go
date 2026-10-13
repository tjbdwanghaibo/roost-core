package skillcompose

// FeatureKey 标识一个可组合的技能特征，FeatureKind 是特征的类别，TransformKind 是特征允许的变换。
type FeatureKey string
type FeatureKind string
type TransformKind string

// TransformIdentity 表示不做变换、原样复用特征。
const TransformIdentity TransformKind = "identity"

// FeatureDescriptor 描述 catalog 里的一个特征。
type FeatureDescriptor struct {
	Key                FeatureKey
	Kind               FeatureKind
	IdentityWeight     int
	RequiredCapability []string
	AllowedTransforms  []TransformKind
}

// Catalog 是特征描述表。
type Catalog struct {
	Features map[FeatureKey]FeatureDescriptor
}

// DefaultCatalog 返回内置的默认特征表。
func DefaultCatalog() Catalog {
	features := []FeatureDescriptor{
		{Key: "effect.damage", Kind: "effect", IdentityWeight: 3}, {Key: "effect.heal", Kind: "effect", IdentityWeight: 3},
		{Key: "select.chain", Kind: "select", IdentityWeight: 2}, {Key: "motion.spawn", Kind: "spawn", IdentityWeight: 2},
	}
	result := Catalog{Features: make(map[FeatureKey]FeatureDescriptor, len(features))}
	for _, feature := range features {
		result.Features[feature.Key] = feature
	}
	return result
}
