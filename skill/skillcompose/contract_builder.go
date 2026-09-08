package skillcompose

import (
	"errors"
	"sort"

	"github.com/tjbdwanghaibo/roost-skill/skill"
)

var (
	ErrSourceProfileRequired = errors.New("skillcompose: at least one source profile is required")
	ErrContractInvalid       = errors.New("skillcompose: composition contract is invalid")
)

func BuildContract(profiles []SkillProfile, authority skill.AuthorityIdentity, policy CompositionPolicy, caller CallerPolicy) (SkillCompositionContract, error) {
	if len(profiles) == 0 {
		return SkillCompositionContract{}, ErrSourceProfileRequired
	}
	if authority.Revision == "" || authority.Digest == "" || policy.ID == "" {
		return SkillCompositionContract{}, ErrContractInvalid
	}
	if !metricsNonNegative(policy.Maximum) || !metricsNonNegative(caller.Maximum) {
		return SkillCompositionContract{}, ErrContractInvalid
	}
	contract := SkillCompositionContract{Version: "skillcompose/v2", Authority: authority, Policy: PolicyIdentity{ID: policy.ID}, Constraints: []Constraint{{Key: "commit_order_preserved"}, {Key: "passive_dispatch_bound"}, {Key: "quantity_compatible"}, {Key: "random_site_bound"}}}
	seenSources := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if profile.SkillID == "" || profile.GameplayDigest == "" {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		if _, duplicate := seenSources[profile.SkillID]; duplicate {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		seenSources[profile.SkillID] = struct{}{}
		if profile.Authority != authority {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		contract.Sources = append(contract.Sources, SourceIdentity{SkillID: profile.SkillID, GameplayDigest: profile.GameplayDigest})
		seenFeatures := make(map[FeatureKey]struct{}, len(profile.Features))
		for _, feature := range profile.Features {
			if feature == "" {
				return SkillCompositionContract{}, ErrContractInvalid
			}
			if _, duplicate := seenFeatures[feature]; duplicate {
				continue
			}
			seenFeatures[feature] = struct{}{}
			contract.Grants = append(contract.Grants, SourceGrant{SourceID: profile.SkillID, Feature: feature, AllowedTransforms: []TransformKind{TransformIdentity}})
		}
		var ok bool
		contract.Budgets.Targets, ok = addNonNegative(contract.Budgets.Targets, profile.Metrics.Targets)
		if !ok {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		contract.Budgets.Processes, ok = addNonNegative(contract.Budgets.Processes, profile.Metrics.Processes)
		if !ok {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		contract.Budgets.Mutations, ok = addNonNegative(contract.Budgets.Mutations, profile.Metrics.Mutations)
		if !ok {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		contract.Budgets.EventsPerRoot, ok = addNonNegative(contract.Budgets.EventsPerRoot, profile.Metrics.EventsPerRoot)
		if !ok {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		contract.Budgets.RandomSites, ok = addNonNegative(contract.Budgets.RandomSites, profile.Metrics.RandomSites)
		if !ok {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		if profile.Metrics.LifetimeTicks < 0 || contract.Budgets.LifetimeTicks > skill.Tick(^uint64(0)>>1)-profile.Metrics.LifetimeTicks {
			return SkillCompositionContract{}, ErrContractInvalid
		}
		contract.Budgets.LifetimeTicks += profile.Metrics.LifetimeTicks
	}
	contract.Budgets = tightenBudgets(contract.Budgets, policy.Maximum)
	contract.Budgets = tightenBudgets(contract.Budgets, caller.Maximum)
	if policy.AllowGenericPackages && !caller.DisableGenericPackages {
		contract.Packages = []GenericPackageGrant{{Key: "generic"}}
	}
	_, digest, err := CanonicalContract(contract)
	if err != nil {
		return SkillCompositionContract{}, err
	}
	contract.Digest = digest
	if err := ValidateContract(contract); err != nil {
		return SkillCompositionContract{}, err
	}
	return contract, nil
}
func tightenBudgets(value CompositionBudgets, limit Metrics) CompositionBudgets {
	if limit.Targets > 0 && value.Targets > limit.Targets {
		value.Targets = limit.Targets
	}
	if limit.Processes > 0 && value.Processes > limit.Processes {
		value.Processes = limit.Processes
	}
	if limit.Mutations > 0 && value.Mutations > limit.Mutations {
		value.Mutations = limit.Mutations
	}
	if limit.EventsPerRoot > 0 && value.EventsPerRoot > limit.EventsPerRoot {
		value.EventsPerRoot = limit.EventsPerRoot
	}
	if limit.RandomSites > 0 && value.RandomSites > limit.RandomSites {
		value.RandomSites = limit.RandomSites
	}
	if limit.LifetimeTicks > 0 && value.LifetimeTicks > limit.LifetimeTicks {
		value.LifetimeTicks = limit.LifetimeTicks
	}
	return value
}

func addNonNegative(left, right int) (int, bool) {
	if right < 0 || left > int(^uint(0)>>1)-right {
		return 0, false
	}
	return left + right, true
}

func metricsNonNegative(value Metrics) bool {
	return value.Targets >= 0 && value.Processes >= 0 && value.Mutations >= 0 && value.EventsPerRoot >= 0 && value.RandomSites >= 0 && value.LifetimeTicks >= 0
}
func sortedFeatures(profiles []SkillProfile) []FeatureKey {
	set := map[FeatureKey]struct{}{}
	for _, profile := range profiles {
		for _, feature := range profile.Features {
			set[feature] = struct{}{}
		}
	}
	result := make([]FeatureKey, 0, len(set))
	for feature := range set {
		result = append(result, feature)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
