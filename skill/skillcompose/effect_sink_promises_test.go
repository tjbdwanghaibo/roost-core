package skillcompose

import (
	"github.com/tjbdwanghaibo/roost-core/skill"
	"testing"
)

func TestGrantedNonDamageEffectsAreValidCandidates(t *testing.T) {
	for _, operation := range []string{"shield", "status", "attribute_modifier", "resource", "teleport", "motion_impulse", "stop_movement", "modify_spawn", "modify_status_instance", "entity_command", "state", "ability_state", "restore_snapshot"} {
		t.Run(operation, func(t *testing.T) {
			authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
			feature := FeatureKey("effect." + operation)
			source := SkillProfile{SkillID: "source", GameplayDigest: "source-digest", Authority: authority, Features: []FeatureKey{feature}, Metrics: Metrics{Mutations: 1}}
			contract, err := BuildContract([]SkillProfile{source}, authority, CompositionPolicy{ID: "policy"}, CallerPolicy{})
			if err != nil {
				t.Fatal(err)
			}
			candidate := SkillProfile{SkillID: "candidate", GameplayDigest: "candidate-digest", Authority: authority, Sources: []SourceIdentity{{SkillID: source.SkillID, GameplayDigest: source.GameplayDigest}}, Features: []FeatureKey{feature}, FeatureOrigins: []FeatureOrigin{{Feature: feature, SourceID: source.SkillID, Transform: TransformIdentity}}, Operations: []string{operation}, Metrics: source.Metrics}
			if report := ValidateCandidate(contract, candidate); !report.Valid {
				t.Fatalf("granted effect %s rejected: %+v", operation, report)
			}
		})
	}
}
