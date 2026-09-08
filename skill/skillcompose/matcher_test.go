package skillcompose

import "testing"

func TestMatcherIsDeterministic(t *testing.T) {
	contract := SkillCompositionContract{Grants: []SourceGrant{{SourceID: "b", Feature: "effect.damage"}, {SourceID: "a", Feature: "effect.damage"}}}
	profile := SkillProfile{Features: []FeatureKey{"effect.damage"}}
	matches := MatchFeatures(contract, profile)
	if len(matches) != 2 || matches[0].SourceID != "a" {
		t.Fatalf("matches=%#v", matches)
	}
}
