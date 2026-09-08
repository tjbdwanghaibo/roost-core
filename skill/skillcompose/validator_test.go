package skillcompose

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-skill/skill"
)

func TestValidateCandidateRejectsUngrantableGrowthAndDisconnectedFlow(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	contract, err := BuildContract([]SkillProfile{{SkillID: "a", GameplayDigest: "source", Authority: authority, Features: []FeatureKey{"effect.damage"}, Metrics: Metrics{Targets: 1, Processes: 1, Mutations: 1, LifetimeTicks: 1}}}, authority, CompositionPolicy{ID: "p"}, CallerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	valid := SkillProfile{SkillID: "candidate", GameplayDigest: "candidate-digest", Authority: authority, Sources: []SourceIdentity{{SkillID: "a", GameplayDigest: "source"}}, Features: []FeatureKey{"effect.damage"}, FeatureOrigins: []FeatureOrigin{{Feature: "effect.damage", SourceID: "a", Transform: TransformIdentity}}, Operations: []string{"damage"}, Metrics: Metrics{Targets: 1, Processes: 1, Mutations: 1, LifetimeTicks: 1}}
	if !ValidateCandidate(contract, valid).Valid {
		t.Fatal("valid candidate rejected")
	}
	invalid := valid
	invalid.Features = []FeatureKey{"effect.heal"}
	invalid.Operations = []string{"finish"}
	invalid.Metrics.Targets = 2
	if ValidateCandidate(contract, invalid).Valid {
		t.Fatal("invalid candidate accepted")
	}
}

func TestValidateCandidateRejectsUngroundedFeatureOrigin(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	contract, err := BuildContract([]SkillProfile{{SkillID: "a", GameplayDigest: "source", Authority: authority, Features: []FeatureKey{"effect.damage"}, Metrics: Metrics{Mutations: 1}}}, authority, CompositionPolicy{ID: "p"}, CallerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	candidate := SkillProfile{SkillID: "candidate", GameplayDigest: "candidate", Authority: authority, Sources: []SourceIdentity{{SkillID: "a", GameplayDigest: "source"}}, Features: []FeatureKey{"effect.damage"}, FeatureOrigins: []FeatureOrigin{{Feature: "effect.damage", SourceID: "substituted", Transform: TransformIdentity}}, Operations: []string{"damage"}, Metrics: Metrics{Mutations: 1}}
	if report := ValidateCandidate(contract, candidate); report.Valid {
		t.Fatalf("ungrounded candidate accepted: %#v", report)
	}
}
