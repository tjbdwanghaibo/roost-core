package skillcompose

import "github.com/tjbdwanghaibo/roost-core/gameplay/skill"

// CompositionPolicy 是组合策略：策略 ID、是否允许通用包与各项上限。
type CompositionPolicy struct {
	ID                   string
	AllowGenericPackages bool
	Maximum              Metrics
}

// CallerPolicy 是调用方附加的收紧条件，只能在 CompositionPolicy 之上进一步收紧。
type CallerPolicy struct {
	Maximum                Metrics
	DisableGenericPackages bool
}

// PolicyIdentity 是契约里记录的策略身份。
type PolicyIdentity struct {
	ID string `json:"id"`
}

// SourceIdentity 是来源技能的 ID 与 gameplay digest。
type SourceIdentity struct {
	SkillID        string `json:"skill_id"`
	GameplayDigest string `json:"gameplay_digest"`
}

// CompositionBudgets 是组合结果的预算上限。
type CompositionBudgets struct {
	Targets       int        `json:"targets"`
	Spawns        int        `json:"spawns"`
	Mutations     int        `json:"mutations"`
	EventsPerRoot int        `json:"events_per_root"`
	RandomSites   int        `json:"random_sites"`
	LifetimeTicks skill.Tick `json:"lifetime_ticks"`
}
