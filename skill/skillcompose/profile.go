package skillcompose

import "github.com/tjbdwanghaibo/roost-skill/skill"

type SkillProfile struct {
	SkillID, GameplayDigest   string
	Authority                 skill.AuthorityIdentity
	Sources                   []SourceIdentity
	FeatureOrigins            []FeatureOrigin
	ActivationMode, InputKind string
	Features                  []FeatureKey
	Operations                []string
	Selections                []SelectionFact
	Metrics                   Metrics
	PresentationDigest        string
}

type FeatureOrigin struct {
	Feature   FeatureKey
	SourceID  string
	Transform TransformKind
}

type SelectionFact struct {
	ElementKind, Shape, ConsumerMode string
	Limit                            int
	HasEmpty                         bool
}
type Metrics struct {
	Targets, Processes, Mutations, EventsPerRoot, RandomSites int
	LifetimeTicks                                             skill.Tick
}
