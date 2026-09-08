package skillcompose

type ValidationReport struct {
	Valid       bool
	Diagnostics []Diagnostic
	Matches     []Match
}

func ValidateCandidate(contract SkillCompositionContract, candidate SkillProfile) ValidationReport {
	report := ValidationReport{Valid: true, Matches: MatchFeatures(contract, candidate)}
	if err := ValidateContract(contract); err != nil {
		report.Valid = false
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "CONTRACT_INVALID", Message: err.Error()})
		return report
	}
	if candidate.SkillID == "" || candidate.GameplayDigest == "" || candidate.Authority != contract.Authority {
		report.Valid = false
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "CANDIDATE_IDENTITY_INVALID", Message: "candidate identity or authority does not match contract"})
	}
	wantSources := make(map[string]string, len(contract.Sources))
	for _, source := range contract.Sources {
		wantSources[source.SkillID] = source.GameplayDigest
	}
	gotSources := make(map[string]string, len(candidate.Sources))
	for _, source := range candidate.Sources {
		if source.SkillID == "" || source.GameplayDigest == "" || gotSources[source.SkillID] != "" {
			report.Valid = false
			continue
		}
		gotSources[source.SkillID] = source.GameplayDigest
	}
	if len(gotSources) != len(wantSources) {
		report.Valid = false
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "PROVENANCE_MISMATCH", Message: "candidate sources do not match contract"})
	} else {
		for id, digest := range wantSources {
			if gotSources[id] != digest {
				report.Valid = false
				report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "PROVENANCE_MISMATCH", Message: "candidate sources do not match contract"})
				break
			}
		}
	}
	allowed := map[FeatureKey]bool{}
	type grantKey struct {
		source  string
		feature FeatureKey
	}
	grantTransforms := make(map[grantKey]map[TransformKind]struct{}, len(contract.Grants))
	for _, grant := range contract.Grants {
		allowed[grant.Feature] = true
		key := grantKey{grant.SourceID, grant.Feature}
		transforms := make(map[TransformKind]struct{}, len(grant.AllowedTransforms))
		for _, transform := range grant.AllowedTransforms {
			transforms[transform] = struct{}{}
		}
		grantTransforms[key] = transforms
	}
	candidateFeatures := make(map[FeatureKey]struct{}, len(candidate.Features))
	seenFeatures := make(map[FeatureKey]struct{}, len(candidate.Features))
	for _, feature := range candidate.Features {
		if feature == "" {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "FEATURE_INVALID", Message: "empty or duplicate feature"})
			continue
		}
		if _, duplicate := seenFeatures[feature]; duplicate {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "FEATURE_INVALID", Message: "empty or duplicate feature"})
			continue
		}
		seenFeatures[feature] = struct{}{}
		candidateFeatures[feature] = struct{}{}
		if !allowed[feature] {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "FEATURE_NOT_GRANTED", Message: string(feature)})
		}
	}
	originCounts := make(map[FeatureKey]int, len(candidate.Features))
	seenOrigins := make(map[FeatureOrigin]struct{}, len(candidate.FeatureOrigins))
	for _, origin := range candidate.FeatureOrigins {
		if origin.Feature == "" || origin.SourceID == "" || origin.Transform == "" {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "FEATURE_ORIGIN_INVALID", Message: "feature origin is incomplete"})
			continue
		}
		if _, duplicate := seenOrigins[origin]; duplicate {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "FEATURE_ORIGIN_INVALID", Message: "duplicate feature origin"})
			continue
		}
		seenOrigins[origin] = struct{}{}
		if _, exists := candidateFeatures[origin.Feature]; !exists {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "FEATURE_ORIGIN_INVALID", Message: "origin references a feature absent from candidate"})
			continue
		}
		transforms := grantTransforms[grantKey{origin.SourceID, origin.Feature}]
		if _, granted := transforms[origin.Transform]; !granted {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "TRANSFORM_NOT_GRANTED", Message: string(origin.Feature)})
			continue
		}
		originCounts[origin.Feature]++
	}
	for feature := range candidateFeatures {
		if originCounts[feature] == 0 {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "FEATURE_ORIGIN_MISSING", Message: string(feature)})
		}
	}
	if candidate.Metrics.Targets < 0 || candidate.Metrics.Processes < 0 || candidate.Metrics.Mutations < 0 || candidate.Metrics.EventsPerRoot < 0 || candidate.Metrics.RandomSites < 0 || candidate.Metrics.LifetimeTicks < 0 || candidate.Metrics.Targets > contract.Budgets.Targets || candidate.Metrics.Processes > contract.Budgets.Processes || candidate.Metrics.Mutations > contract.Budgets.Mutations || candidate.Metrics.EventsPerRoot > contract.Budgets.EventsPerRoot || candidate.Metrics.RandomSites > contract.Budgets.RandomSites || candidate.Metrics.LifetimeTicks > contract.Budgets.LifetimeTicks {
		report.Valid = false
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "BUDGET_EXCEEDED", Message: "candidate exceeds contract budget"})
	}
	if !GraphFromProfile(candidate).ReachesSink() {
		report.Valid = false
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "CAUSAL_DISCONNECTED", Message: "activation does not reach gameplay sink"})
	}
	return report
}
