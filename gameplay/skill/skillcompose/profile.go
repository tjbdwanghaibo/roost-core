package skillcompose

import "github.com/tjbdwanghaibo/roost-core/gameplay/skill"

// SkillProfile 是从已编译 Program 提取的组合用摘要：身份、特征、操作、选择与度量。
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

// FeatureOrigin 记录一个特征来自哪个来源技能、经过什么变换。
type FeatureOrigin struct {
	Feature   FeatureKey
	SourceID  string
	Transform TransformKind
}

// SelectionFact 是一个目标选择位点的摘要。
type SelectionFact struct {
	ElementKind, Shape, ConsumerMode string
	Limit                            int
	HasEmpty                         bool
}

// Metrics 是组合预算对应的各项度量。
type Metrics struct {
	Targets, Spawns, Mutations, EventsPerRoot, RandomSites int
	LifetimeTicks                                          skill.Tick
}
