package skillcompose

import "sort"

// PromptContractView 是可以给生成器看的契约视图：允许的特征、来源与表现 digest。
type PromptContractView struct {
	AllowedFeatures     []FeatureKey
	AllowedOrigins      []FeatureOrigin
	PresentationDigests []string
}

// DerivePromptView 直接从来源 profile 推出生成器视图（每个特征只允许原样复用）。
func DerivePromptView(profiles []SkillProfile) PromptContractView {
	result := PromptContractView{AllowedFeatures: sortedFeatures(profiles)}
	for _, profile := range profiles {
		result.PresentationDigests = append(result.PresentationDigests, profile.PresentationDigest)
		for _, feature := range profile.Features {
			result.AllowedOrigins = append(result.AllowedOrigins, FeatureOrigin{Feature: feature, SourceID: profile.SkillID, Transform: TransformIdentity})
		}
	}
	sortFeatureOrigins(result.AllowedOrigins)
	return result
}

// DeriveContractPromptView 只把 digest 覆盖到的授予暴露给生成器；契约无效时返回 ValidateContract 的错误。
func DeriveContractPromptView(contract SkillCompositionContract) (PromptContractView, error) {
	if err := ValidateContract(contract); err != nil {
		return PromptContractView{}, err
	}
	result := PromptContractView{}
	features := make(map[FeatureKey]struct{})
	for _, grant := range contract.Grants {
		features[grant.Feature] = struct{}{}
		for _, transform := range grant.AllowedTransforms {
			result.AllowedOrigins = append(result.AllowedOrigins, FeatureOrigin{Feature: grant.Feature, SourceID: grant.SourceID, Transform: transform})
		}
	}
	for feature := range features {
		result.AllowedFeatures = append(result.AllowedFeatures, feature)
	}
	sort.Slice(result.AllowedFeatures, func(i, j int) bool { return result.AllowedFeatures[i] < result.AllowedFeatures[j] })
	sortFeatureOrigins(result.AllowedOrigins)
	return result, nil
}

func sortFeatureOrigins(values []FeatureOrigin) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Feature != values[j].Feature {
			return values[i].Feature < values[j].Feature
		}
		if values[i].SourceID != values[j].SourceID {
			return values[i].SourceID < values[j].SourceID
		}
		return values[i].Transform < values[j].Transform
	})
}
