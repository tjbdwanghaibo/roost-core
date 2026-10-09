package skillcompose

import "sort"

type PromptContractView struct {
	AllowedFeatures     []FeatureKey
	AllowedOrigins      []FeatureOrigin
	PresentationDigests []string
}

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

// DeriveContractPromptView exposes only digest-covered grants to generators.
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
