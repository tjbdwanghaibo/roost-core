package skillcompose

type FeatureKey string
type FeatureKind string
type TransformKind string

const TransformIdentity TransformKind = "identity"

type FeatureDescriptor struct {
	Key                FeatureKey
	Kind               FeatureKind
	IdentityWeight     int
	RequiredCapability []string
	AllowedTransforms  []TransformKind
}

type Catalog struct {
	Features map[FeatureKey]FeatureDescriptor
}

func DefaultCatalog() Catalog {
	features := []FeatureDescriptor{
		{Key: "effect.damage", Kind: "effect", IdentityWeight: 3}, {Key: "effect.heal", Kind: "effect", IdentityWeight: 3},
		{Key: "select.chain", Kind: "select", IdentityWeight: 2}, {Key: "motion.process", Kind: "process", IdentityWeight: 2},
	}
	result := Catalog{Features: make(map[FeatureKey]FeatureDescriptor, len(features))}
	for _, feature := range features {
		result.Features[feature.Key] = feature
	}
	return result
}
