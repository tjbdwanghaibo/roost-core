package skillcompose

import (
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

func TestContractCallerPolicyOnlyTightens(t *testing.T) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	profiles := []SkillProfile{{SkillID: "b", GameplayDigest: "b", Authority: authority, Features: []FeatureKey{"effect.damage"}, Metrics: Metrics{Targets: 4, Spawns: 2, Mutations: 9, LifetimeTicks: 8}}, {SkillID: "a", GameplayDigest: "a", Authority: authority, Features: []FeatureKey{"select.chain"}, Metrics: Metrics{Targets: 3, Spawns: 1, Mutations: 2, LifetimeTicks: 4}}}
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

func validProfiles() ([]SkillProfile, skill.AuthorityIdentity) {
	authority := skill.AuthorityIdentity{Revision: "r", Digest: "d"}
	return []SkillProfile{
		{SkillID: "a", GameplayDigest: "da", Authority: authority, Features: []FeatureKey{"effect.damage"}, Metrics: Metrics{Targets: 2, LifetimeTicks: 4}},
		{SkillID: "b", GameplayDigest: "db", Authority: authority, Features: []FeatureKey{"select.chain"}, Metrics: Metrics{Targets: 1, LifetimeTicks: 2}},
	}, authority
}

// BuildContract refuses inputs a contract cannot be minted from; each rule is
// pinned by sentinel and by a single-field mutation of an otherwise valid
// input, so a dropped rule turns one case red.
func TestBuildContractRefusesEachInvalidInput(t *testing.T) {
	profiles, authority := validProfiles()
	policy := CompositionPolicy{ID: "server", Maximum: Metrics{Targets: 10}}
	if _, err := BuildContract(profiles, authority, policy, CallerPolicy{}); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	if _, err := BuildContract(nil, authority, policy, CallerPolicy{}); !errors.Is(err, ErrSourceProfileRequired) {
		t.Fatalf("no profiles = %v", err)
	}
	cases := map[string]func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy){
		"blank authority revision": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			a := skill.AuthorityIdentity{Revision: "", Digest: "d"}
			p := profiles[0]
			p.Authority = a
			return p, a, policy, CallerPolicy{}
		},
		"blank policy id": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			return profiles[0], authority, CompositionPolicy{}, CallerPolicy{}
		},
		"negative policy maximum": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			return profiles[0], authority, CompositionPolicy{ID: "server", Maximum: Metrics{Targets: -1}}, CallerPolicy{}
		},
		"negative caller maximum": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			return profiles[0], authority, policy, CallerPolicy{Maximum: Metrics{Spawns: -1}}
		},
		"profile without skill id": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			p := profiles[0]
			p.SkillID = ""
			return p, authority, policy, CallerPolicy{}
		},
		"profile without gameplay digest": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			p := profiles[0]
			p.GameplayDigest = ""
			return p, authority, policy, CallerPolicy{}
		},
		"blank feature": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			p := profiles[0]
			p.Features = []FeatureKey{""}
			return p, authority, policy, CallerPolicy{}
		},
		"negative lifetime": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			p := profiles[0]
			p.Metrics.LifetimeTicks = -1
			return p, authority, policy, CallerPolicy{}
		},
		"negative targets": func() (SkillProfile, skill.AuthorityIdentity, CompositionPolicy, CallerPolicy) {
			p := profiles[0]
			p.Metrics.Targets = -1
			return p, authority, policy, CallerPolicy{}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			p, a, pol, caller := build()
			if _, err := BuildContract([]SkillProfile{p}, a, pol, caller); !errors.Is(err, ErrContractInvalid) {
				t.Fatalf("BuildContract = %v, want ErrContractInvalid", err)
			}
		})
	}
	dup := []SkillProfile{profiles[0], profiles[0]}
	if _, err := BuildContract(dup, authority, policy, CallerPolicy{}); !errors.Is(err, ErrContractInvalid) {
		t.Fatalf("duplicate skill id = %v", err)
	}
}

// resign recomputes the digest after a deliberate mutation, so ValidateContract
// can only refuse for the rule under test — not for the digest that the
// mutation broke.
func resign(t *testing.T, c SkillCompositionContract) SkillCompositionContract {
	t.Helper()
	_, digest, err := CanonicalContract(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Digest = digest
	return c
}

func TestValidateContractRefusesEachStructuralDefect(t *testing.T) {
	profiles, authority := validProfiles()
	base, err := BuildContract(profiles, authority, CompositionPolicy{ID: "server", AllowGenericPackages: true}, CallerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	base.Obligations = []SourceObligation{{SourceID: "a", Key: "keep-order"}}
	base = resign(t, base)
	if err := ValidateContract(base); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := map[string]func(*SkillCompositionContract){
		"foreign version":          func(c *SkillCompositionContract) { c.Version = "skillcompose/v1" },
		"blank authority":          func(c *SkillCompositionContract) { c.Authority.Digest = "" },
		"blank policy":             func(c *SkillCompositionContract) { c.Policy.ID = "" },
		"no sources":               func(c *SkillCompositionContract) { c.Sources = nil },
		"negative budget":          func(c *SkillCompositionContract) { c.Budgets.Mutations = -1 },
		"source without digest":    func(c *SkillCompositionContract) { c.Sources[0].GameplayDigest = "" },
		"duplicate source":         func(c *SkillCompositionContract) { c.Sources = append(c.Sources, c.Sources[0]) },
		"grant for unknown source": func(c *SkillCompositionContract) { c.Grants[0].SourceID = "ghost" },
		"grant without feature":    func(c *SkillCompositionContract) { c.Grants[0].Feature = "" },
		"duplicate grant":          func(c *SkillCompositionContract) { c.Grants = append(c.Grants, c.Grants[0]) },
		"grant without transforms": func(c *SkillCompositionContract) { c.Grants[0].AllowedTransforms = nil },
		"blank transform":          func(c *SkillCompositionContract) { c.Grants[0].AllowedTransforms = []TransformKind{""} },
		"duplicate transform": func(c *SkillCompositionContract) {
			c.Grants[0].AllowedTransforms = []TransformKind{TransformIdentity, TransformIdentity}
		},
		"obligation for unknown source": func(c *SkillCompositionContract) { c.Obligations[0].SourceID = "ghost" },
		"obligation without key":        func(c *SkillCompositionContract) { c.Obligations[0].Key = "" },
		"duplicate obligation":          func(c *SkillCompositionContract) { c.Obligations = append(c.Obligations, c.Obligations[0]) },
		"package without key":           func(c *SkillCompositionContract) { c.Packages = []GenericPackageGrant{{}} },
		"duplicate package":             func(c *SkillCompositionContract) { c.Packages = append(c.Packages, c.Packages[0]) },
		"constraint without key":        func(c *SkillCompositionContract) { c.Constraints[0].Key = "" },
		"duplicate constraint":          func(c *SkillCompositionContract) { c.Constraints = append(c.Constraints, c.Constraints[0]) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base
			c.Sources = append([]SourceIdentity(nil), base.Sources...)
			c.Grants = append([]SourceGrant(nil), base.Grants...)
			c.Obligations = append([]SourceObligation(nil), base.Obligations...)
			c.Packages = append([]GenericPackageGrant(nil), base.Packages...)
			c.Constraints = append([]Constraint(nil), base.Constraints...)
			mutate(&c)
			c = resign(t, c)
			if err := ValidateContract(c); !errors.Is(err, ErrContractInvalid) {
				t.Fatalf("ValidateContract = %v, want ErrContractInvalid", err)
			}
		})
	}
	tampered := base
	tampered.Digest = "not-the-digest"
	if err := ValidateContract(tampered); !errors.Is(err, ErrContractInvalid) || err.Error() != "skillcompose: composition contract is invalid: digest mismatch" {
		t.Fatalf("digest mismatch = %v", err)
	}
}
