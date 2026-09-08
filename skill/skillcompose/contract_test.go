package skillcompose

import (
	"github.com/tjbdwanghaibo/roost-skill/skill"
	"strings"
	"testing"
)

func TestContractCallerPolicyOnlyTightens(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	profiles := []SkillProfile{{SkillID: "b", GameplayDigest: "b", Authority: authority, Features: []FeatureKey{"effect.damage"}, Metrics: Metrics{Targets: 4, Processes: 2, Mutations: 9, LifetimeTicks: 8}}, {SkillID: "a", GameplayDigest: "a", Authority: authority, Features: []FeatureKey{"select.chain"}, Metrics: Metrics{Targets: 3, Processes: 1, Mutations: 2, LifetimeTicks: 4}}}
	contract, err := BuildContract(profiles, authority, CompositionPolicy{ID: "server", AllowGenericPackages: true, Maximum: Metrics{Targets: 6}}, CallerPolicy{Maximum: Metrics{Targets: 5}, DisableGenericPackages: true})
	if err != nil {
		t.Fatal(err)
	}
	if contract.Budgets.Targets != 5 || len(contract.Packages) != 0 {
		t.Fatalf("contract=%#v", contract)
	}
	if len(DerivePromptView(profiles).AllowedFeatures) != 2 {
		t.Fatal("prompt view missed grants")
	}
}
func TestContractCanonicalOmitsDerivedPromptFields(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	contract, err := BuildContract([]SkillProfile{{SkillID: "a", GameplayDigest: "d", Authority: authority}}, authority, CompositionPolicy{ID: "p"}, CallerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	data, digest, err := CanonicalContract(contract)
	if err != nil || digest != contract.Digest {
		t.Fatalf("canonical=%s digest=%s err=%v", data, digest, err)
	}
	for _, forbidden := range []string{"allowed_feature_union", "prompt_projection", "repair_hints"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal(forbidden)
		}
	}
}

func TestCanonicalContractDoesNotMutateCaller(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	contract, err := BuildContract([]SkillProfile{{SkillID: "b", GameplayDigest: "2", Authority: authority, Features: []FeatureKey{"z", "a"}}}, authority, CompositionPolicy{ID: "p"}, CallerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	beforeSource, beforeGrant := contract.Sources[0], contract.Grants[0]
	if _, _, err := CanonicalContract(contract); err != nil {
		t.Fatal(err)
	}
	if contract.Sources[0] != beforeSource || contract.Grants[0].Feature != beforeGrant.Feature {
		t.Fatal("canonicalization mutated caller-owned slices")
	}
}

func TestBuildContractRejectsUnboundSourceAuthority(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	_, err := BuildContract([]SkillProfile{{SkillID: "a", GameplayDigest: "d"}}, authority, CompositionPolicy{ID: "p"}, CallerPolicy{})
	if err == nil {
		t.Fatal("unbound source authority accepted")
	}
}
