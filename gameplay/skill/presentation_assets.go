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

// VisualAsset 是一个领域表现在渲染端的解析结果。技能定义刻意不含引擎路径，路径留在 VisualAssetResolver 背后
// 带版本的 catalog 里。
type VisualAsset struct {
	Key         string
	FallbackKey string
	Preload     []string
}

// VisualAssetResolver 把清单绑定到唯一一个 catalog 修订；digest 防止部署时误复用了同一个修订标签。
type VisualAssetResolver interface {
	CatalogIdentity() (revision string, digest string)
	Resolve(VisualView) (VisualAsset, error)
}

// ResolvedVisualAsset 是清单里一项表现解析后的资源。
type ResolvedVisualAsset struct {
	Visual VisualView
	Asset  VisualAsset
}

// ResolvedPresentationPlan 是整份表现计划解析后的结果。
type ResolvedPresentationPlan struct {
	Plan   PresentationPlan
	Assets []ResolvedVisualAsset
}

// ResolvePresentationPlan 先校验完整清单，再返回任何渲染绑定：catalog 发布出错时整体失败，而不是渲染出半个技能。
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
	for _, mount := range append(append([]PresentationMount(nil), result.Plan.Effects...), result.Plan.Spawns...) {
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
	plan.Spawns = append([]PresentationMount(nil), plan.Spawns...)
	return plan
}

func cloneVisualView(visual VisualView) VisualView {
	visual.Elements = append([]string(nil), visual.Elements...)
	return visual
}
