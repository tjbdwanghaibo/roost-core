package skillcompose

import "github.com/tjbdwanghaibo/roost-core/gameplay/skill"

// SkillCompositionContract 是绑定权威身份的组合契约：来源技能、按来源授予的特征、义务、通用包、约束与预算；
// Digest 由 CanonicalContract 算出。
type SkillCompositionContract struct {
	Version     string                  `json:"version"`
	Sources     []SourceIdentity        `json:"sources"`
	Authority   skill.AuthorityIdentity `json:"authority"`
	Policy      PolicyIdentity          `json:"policy"`
	Grants      []SourceGrant           `json:"grants"`
	Obligations []SourceObligation      `json:"obligations"`
	Packages    []GenericPackageGrant   `json:"packages"`
	Constraints []Constraint            `json:"constraints"`
	Budgets     CompositionBudgets      `json:"budgets"`
	Digest      string                  `json:"digest"`
}

// SourceGrant 授予组合结果使用某个来源技能的一个特征及允许的变换。
type SourceGrant struct {
	SourceID          string          `json:"source_id"`
	Feature           FeatureKey      `json:"feature"`
	AllowedTransforms []TransformKind `json:"allowed_transforms"`
}

// SourceObligation 是组合结果对某个来源技能必须满足的义务。
type SourceObligation struct {
	SourceID string `json:"source_id"`
	Key      string `json:"key"`
}

// GenericPackageGrant 授予使用一个通用包。
type GenericPackageGrant struct {
	Key string `json:"key"`
}

// Constraint 是组合结果必须保持的一条约束。
type Constraint struct {
	Key string `json:"key"`
}
