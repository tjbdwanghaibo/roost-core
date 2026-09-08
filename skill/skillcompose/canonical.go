package skillcompose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func CanonicalContract(contract SkillCompositionContract) ([]byte, string, error) {
	contract.Sources = append([]SourceIdentity(nil), contract.Sources...)
	contract.Grants = append([]SourceGrant(nil), contract.Grants...)
	contract.Obligations = append([]SourceObligation(nil), contract.Obligations...)
	contract.Packages = append([]GenericPackageGrant(nil), contract.Packages...)
	contract.Constraints = append([]Constraint(nil), contract.Constraints...)
	for index := range contract.Grants {
		contract.Grants[index].AllowedTransforms = append([]TransformKind(nil), contract.Grants[index].AllowedTransforms...)
	}
	contract.Digest = ""
	sort.Slice(contract.Sources, func(i, j int) bool { return contract.Sources[i].SkillID < contract.Sources[j].SkillID })
	sort.Slice(contract.Grants, func(i, j int) bool {
		if contract.Grants[i].SourceID != contract.Grants[j].SourceID {
			return contract.Grants[i].SourceID < contract.Grants[j].SourceID
		}
		return contract.Grants[i].Feature < contract.Grants[j].Feature
	})
	for index := range contract.Grants {
		sort.Slice(contract.Grants[index].AllowedTransforms, func(i, j int) bool {
			return contract.Grants[index].AllowedTransforms[i] < contract.Grants[index].AllowedTransforms[j]
		})
	}
	sort.Slice(contract.Obligations, func(i, j int) bool {
		if contract.Obligations[i].SourceID != contract.Obligations[j].SourceID {
			return contract.Obligations[i].SourceID < contract.Obligations[j].SourceID
		}
		return contract.Obligations[i].Key < contract.Obligations[j].Key
	})
	sort.Slice(contract.Packages, func(i, j int) bool { return contract.Packages[i].Key < contract.Packages[j].Key })
	sort.Slice(contract.Constraints, func(i, j int) bool { return contract.Constraints[i].Key < contract.Constraints[j].Key })
	data, err := json.Marshal(contract)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]), nil
}
