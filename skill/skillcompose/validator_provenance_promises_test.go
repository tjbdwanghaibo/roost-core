package skillcompose

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/skill"
)

// RR-20261005-NC-154：ValidateCandidate 的每个拒绝都要带诊断。空或重复的
// candidate source 在旧实现里只置 Valid=false 就跳过，去重后的集合与合同相同时
// 报告既 invalid 又没有任何诊断，生成器拿不到失败原因。
func TestValidateCandidateExplainsBlankOrDuplicateSources(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	contract, err := BuildContract([]SkillProfile{{SkillID: "a", GameplayDigest: "source", Authority: authority, Features: []FeatureKey{"effect.damage"}, Metrics: Metrics{Targets: 1, Processes: 1, Mutations: 1, LifetimeTicks: 1}}}, authority, CompositionPolicy{ID: "p"}, CallerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	valid := SkillProfile{SkillID: "candidate", GameplayDigest: "candidate-digest", Authority: authority, Sources: []SourceIdentity{{SkillID: "a", GameplayDigest: "source"}}, Features: []FeatureKey{"effect.damage"}, FeatureOrigins: []FeatureOrigin{{Feature: "effect.damage", SourceID: "a", Transform: TransformIdentity}}, Operations: []string{"damage"}, Metrics: Metrics{Targets: 1, Processes: 1, Mutations: 1, LifetimeTicks: 1}}
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
