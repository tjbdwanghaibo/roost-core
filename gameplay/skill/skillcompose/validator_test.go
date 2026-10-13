package skillcompose

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

func TestValidateCandidateRejectsUngrantableGrowthAndDisconnectedFlow(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	contract, err := BuildContract([]SkillProfile{{SkillID: "a", GameplayDigest: "source", Authority: authority, Features: []FeatureKey{"effect.damage"}, Metrics: Metrics{Targets: 1, Spawns: 1, Mutations: 1, LifetimeTicks: 1}}}, authority, CompositionPolicy{ID: "p"}, CallerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	valid := SkillProfile{SkillID: "candidate", GameplayDigest: "candidate-digest", Authority: authority, Sources: []SourceIdentity{{SkillID: "a", GameplayDigest: "source"}}, Features: []FeatureKey{"effect.damage"}, FeatureOrigins: []FeatureOrigin{{Feature: "effect.damage", SourceID: "a", Transform: TransformIdentity}}, Operations: []string{"damage"}, Metrics: Metrics{Targets: 1, Spawns: 1, Mutations: 1, LifetimeTicks: 1}}
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

// ValidateCandidate 的每个拒绝都带诊断：空或重复的 candidate source 不能只置 Valid=false 而没有任何诊断，
// 生成器要拿到失败原因。
func TestValidateCandidateExplainsBlankOrDuplicateSources(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	contract, err := BuildContract([]SkillProfile{{SkillID: "a", GameplayDigest: "source", Authority: authority, Features: []FeatureKey{"effect.damage"}, Metrics: Metrics{Targets: 1, Spawns: 1, Mutations: 1, LifetimeTicks: 1}}}, authority, CompositionPolicy{ID: "p"}, CallerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	valid := SkillProfile{SkillID: "candidate", GameplayDigest: "candidate-digest", Authority: authority, Sources: []SourceIdentity{{SkillID: "a", GameplayDigest: "source"}}, Features: []FeatureKey{"effect.damage"}, FeatureOrigins: []FeatureOrigin{{Feature: "effect.damage", SourceID: "a", Transform: TransformIdentity}}, Operations: []string{"damage"}, Metrics: Metrics{Targets: 1, Spawns: 1, Mutations: 1, LifetimeTicks: 1}}
	if report := ValidateCandidate(contract, valid); !report.Valid || len(report.Diagnostics) != 0 {
		t.Fatalf("valid candidate = %#v", report)
	}
	for name, sources := range map[string][]SourceIdentity{
		"duplicate source": {{SkillID: "a", GameplayDigest: "source"}, {SkillID: "a", GameplayDigest: "source"}},
		"blank source id":  {{SkillID: "a", GameplayDigest: "source"}, {SkillID: "", GameplayDigest: "x"}},
		"blank digest":     {{SkillID: "a", GameplayDigest: "source"}, {SkillID: "b", GameplayDigest: ""}},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Sources = sources
			report := ValidateCandidate(contract, candidate)
			if report.Valid {
				t.Fatalf("candidate accepted: %#v", report)
			}
			for _, diagnostic := range report.Diagnostics {
				if diagnostic.Code == "PROVENANCE_MISMATCH" {
					return
				}
			}
			t.Fatalf("invalid report without PROVENANCE_MISMATCH: %#v", report.Diagnostics)
		})
	}
}

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
