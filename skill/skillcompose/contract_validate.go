package skillcompose

import (
	"errors"
	"fmt"
)

func ValidateContract(contract SkillCompositionContract) error {
	if contract.Version != "skillcompose/v2" || contract.Authority.Revision == "" || contract.Authority.Digest == "" || contract.Policy.ID == "" || len(contract.Sources) == 0 || contract.Digest == "" {
		return ErrContractInvalid
	}
	if contract.Budgets.Targets < 0 || contract.Budgets.Processes < 0 || contract.Budgets.Mutations < 0 || contract.Budgets.EventsPerRoot < 0 || contract.Budgets.RandomSites < 0 || contract.Budgets.LifetimeTicks < 0 {
		return ErrContractInvalid
	}
	sources := make(map[string]string, len(contract.Sources))
	for _, source := range contract.Sources {
		if source.SkillID == "" || source.GameplayDigest == "" || sources[source.SkillID] != "" {
			return ErrContractInvalid
		}
		sources[source.SkillID] = source.GameplayDigest
	}
	grants := make(map[string]struct{}, len(contract.Grants))
	for _, grant := range contract.Grants {
		if sources[grant.SourceID] == "" || grant.Feature == "" {
			return ErrContractInvalid
		}
		key := grant.SourceID + "\x00" + string(grant.Feature)
		if _, duplicate := grants[key]; duplicate {
			return ErrContractInvalid
		}
		grants[key] = struct{}{}
		if len(grant.AllowedTransforms) == 0 {
			return ErrContractInvalid
		}
		transforms := make(map[TransformKind]struct{}, len(grant.AllowedTransforms))
		for _, transform := range grant.AllowedTransforms {
			if transform == "" {
				return ErrContractInvalid
			}
			if _, duplicate := transforms[transform]; duplicate {
				return ErrContractInvalid
			}
			transforms[transform] = struct{}{}
		}
	}
	obligations := make(map[string]struct{}, len(contract.Obligations))
	for _, obligation := range contract.Obligations {
		if sources[obligation.SourceID] == "" || obligation.Key == "" {
			return ErrContractInvalid
		}
		key := obligation.SourceID + "\x00" + obligation.Key
		if _, duplicate := obligations[key]; duplicate {
			return ErrContractInvalid
		}
		obligations[key] = struct{}{}
	}
	packages := make(map[string]struct{}, len(contract.Packages))
	for _, item := range contract.Packages {
		if item.Key == "" {
			return ErrContractInvalid
		}
		if _, duplicate := packages[item.Key]; duplicate {
			return ErrContractInvalid
		}
		packages[item.Key] = struct{}{}
	}
	constraints := make(map[string]struct{}, len(contract.Constraints))
	for _, item := range contract.Constraints {
		if item.Key == "" {
			return ErrContractInvalid
		}
		if _, duplicate := constraints[item.Key]; duplicate {
			return ErrContractInvalid
		}
		constraints[item.Key] = struct{}{}
	}
	_, digest, err := CanonicalContract(contract)
	if err != nil {
		return errors.Join(ErrContractInvalid, err)
	}
	if digest != contract.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrContractInvalid)
	}
	return nil
}
