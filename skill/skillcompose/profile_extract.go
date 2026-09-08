package skillcompose

import (
	"errors"
	"sort"

	"github.com/tjbdwanghaibo/roost-skill/skill"
)

var ErrProgramRequired = errors.New("skillcompose: compiled program is required")

func ExtractProfile(program *skill.Program) (SkillProfile, error) {
	if program == nil {
		return SkillProfile{}, ErrProgramRequired
	}
	view := skill.Inspect(program)
	profile := SkillProfile{SkillID: view.ID, GameplayDigest: view.Identity.GameplayDigest, Authority: view.Authority, Sources: []SourceIdentity{{SkillID: view.ID, GameplayDigest: view.Identity.GameplayDigest}}, PresentationDigest: view.Identity.PresentationDigest, ActivationMode: view.Cast.Mode, InputKind: view.Input.Kind,
		Metrics: Metrics{Targets: view.Limits.Targets, Processes: view.Limits.Processes, Mutations: view.Limits.Mutations, EventsPerRoot: view.Limits.EventsPerRoot, RandomSites: view.Limits.RandomSites, LifetimeTicks: view.Limits.LifetimeTicks}}
	featureSet := map[FeatureKey]struct{}{}
	for _, operation := range view.Operations {
		profile.Operations = append(profile.Operations, operation.Kind)
		featureSet[FeatureKey("effect."+operation.Kind)] = struct{}{}
	}
	for _, selector := range view.Selectors {
		profile.Selections = append(profile.Selections, SelectionFact{ElementKind: selector.ElementKind, Shape: selector.ShapeKind, ConsumerMode: selector.ConsumerMode, Limit: selector.Limit, HasEmpty: selector.EmptyRoot != 0})
		featureSet[FeatureKey("select."+selector.ShapeKind)] = struct{}{}
	}
	for feature := range featureSet {
		profile.Features = append(profile.Features, feature)
		profile.FeatureOrigins = append(profile.FeatureOrigins, FeatureOrigin{Feature: feature, SourceID: profile.SkillID, Transform: TransformIdentity})
	}
	sort.Slice(profile.Features, func(i, j int) bool { return profile.Features[i] < profile.Features[j] })
	sort.Slice(profile.FeatureOrigins, func(i, j int) bool { return profile.FeatureOrigins[i].Feature < profile.FeatureOrigins[j].Feature })
	return profile, nil
}
