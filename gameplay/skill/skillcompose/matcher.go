package skillcompose

import "sort"

type Match struct {
	SourceID  string
	Feature   FeatureKey
	Operation string
}

func MatchFeatures(contract SkillCompositionContract, profile SkillProfile) []Match {
	grants := append([]SourceGrant(nil), contract.Grants...)
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].SourceID != grants[j].SourceID {
			return grants[i].SourceID < grants[j].SourceID
		}
		return grants[i].Feature < grants[j].Feature
	})
	features := map[FeatureKey]bool{}
	for _, feature := range profile.Features {
		features[feature] = true
	}
	result := []Match{}
	for _, grant := range grants {
		if features[grant.Feature] {
			result = append(result, Match{SourceID: grant.SourceID, Feature: grant.Feature})
		}
	}
	return result
}
