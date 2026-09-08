package skill

import (
	"errors"
	"fmt"
)

var (
	ErrVisualCatalogMismatch = errors.New("skill: visual catalog identity mismatch")
	ErrVisualAssetMissing    = errors.New("skill: visual asset is missing")
	ErrVisualMountInvalid    = errors.New("skill: presentation mount references an invalid visual")
)

// VisualAsset is the renderer-owned resolution of one domain visual. Skill
// definitions deliberately contain no engine paths; those remain in the
// versioned catalog behind VisualAssetResolver.
type VisualAsset struct {
	Key         string
	FallbackKey string
	Preload     []string
}

// VisualAssetResolver binds a manifest to exactly one catalog revision. The
// digest protects deployments where a revision label was accidentally reused.
type VisualAssetResolver interface {
	CatalogIdentity() (revision string, digest string)
	Resolve(VisualView) (VisualAsset, error)
}

type ResolvedVisualAsset struct {
	Visual VisualView
	Asset  VisualAsset
}

type ResolvedPresentationPlan struct {
	Plan   PresentationPlan
	Assets []ResolvedVisualAsset
}

// ResolvePresentationPlan validates the complete manifest before returning any
// renderer bindings. This makes catalog rollout failures fail closed rather
// than producing partially rendered skills.
func ResolvePresentationPlan(plan PresentationPlan, resolver VisualAssetResolver) (ResolvedPresentationPlan, error) {
	if resolver == nil {
		return ResolvedPresentationPlan{}, ErrVisualAssetMissing
	}
	revision, digest := resolver.CatalogIdentity()
	if revision != plan.Manifest.CatalogRevision || digest != plan.Manifest.CatalogDigest {
		return ResolvedPresentationPlan{}, fmt.Errorf("%w: plan=%s/%s resolver=%s/%s", ErrVisualCatalogMismatch, plan.Manifest.CatalogRevision, plan.Manifest.CatalogDigest, revision, digest)
	}
	entries := make(map[VisualIndex]struct{}, len(plan.Manifest.Entries))
	result := ResolvedPresentationPlan{Plan: clonePresentationPlan(plan), Assets: make([]ResolvedVisualAsset, 0, len(plan.Manifest.Entries))}
	for _, visual := range plan.Manifest.Entries {
		if _, exists := entries[visual.Index]; exists {
			return ResolvedPresentationPlan{}, fmt.Errorf("%w: duplicate visual index %d", ErrVisualMountInvalid, visual.Index)
		}
		entries[visual.Index] = struct{}{}
		asset, err := resolver.Resolve(cloneVisualView(visual))
		if err != nil {
			return ResolvedPresentationPlan{}, fmt.Errorf("%w: visual %d: %v", ErrVisualAssetMissing, visual.Index, err)
		}
		if asset.Key == "" {
			return ResolvedPresentationPlan{}, fmt.Errorf("%w: visual %d resolved to an empty key", ErrVisualAssetMissing, visual.Index)
		}
		asset.Preload = append([]string(nil), asset.Preload...)
		result.Assets = append(result.Assets, ResolvedVisualAsset{Visual: cloneVisualView(visual), Asset: asset})
	}
	validateMount := func(mount PresentationMount) error {
		if _, ok := entries[mount.VisualIndex]; !ok {
			return fmt.Errorf("%w: visual %d", ErrVisualMountInvalid, mount.VisualIndex)
		}
		return nil
	}
	if result.Plan.Cast != nil {
		if err := validateMount(*result.Plan.Cast); err != nil {
			return ResolvedPresentationPlan{}, err
		}
	}
	for _, mount := range append(append([]PresentationMount(nil), result.Plan.Effects...), result.Plan.Processes...) {
		if err := validateMount(mount); err != nil {
			return ResolvedPresentationPlan{}, err
		}
	}
	return result, nil
}

func clonePresentationPlan(plan PresentationPlan) PresentationPlan {
	plan.Manifest.Entries = append([]VisualView(nil), plan.Manifest.Entries...)
	for index := range plan.Manifest.Entries {
		plan.Manifest.Entries[index] = cloneVisualView(plan.Manifest.Entries[index])
	}
	if plan.Cast != nil {
		mount := *plan.Cast
		plan.Cast = &mount
	}
	plan.Effects = append([]PresentationMount(nil), plan.Effects...)
	plan.Processes = append([]PresentationMount(nil), plan.Processes...)
	return plan
}

func cloneVisualView(visual VisualView) VisualView {
	visual.Elements = append([]string(nil), visual.Elements...)
	return visual
}
