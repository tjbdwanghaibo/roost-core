package skill

import (
	"errors"
	"testing"
)

type testVisualResolver struct {
	revision string
	digest   string
	fail     bool
}

func (resolver testVisualResolver) CatalogIdentity() (string, string) {
	return resolver.revision, resolver.digest
}

func (resolver testVisualResolver) Resolve(visual VisualView) (VisualAsset, error) {
	if resolver.fail {
		return VisualAsset{}, errors.New("missing")
	}
	return VisualAsset{Key: visual.Category + "/" + visual.Theme, Preload: append([]string(nil), visual.Elements...)}, nil
}

func TestResolvePresentationPlanValidatesCatalogAndDetachesAssets(t *testing.T) {
	plan := PresentationPlan{
		Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Category: "impact", Theme: "default", Elements: []string{"fire"}}}},
		Effects:  []PresentationMount{{VisualIndex: 0, HasEffect: true}},
	}
	resolved, err := ResolvePresentationPlan(plan, testVisualResolver{revision: "r1", digest: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Assets) != 1 || resolved.Assets[0].Asset.Key != "impact/default" {
		t.Fatalf("resolved = %#v", resolved)
	}
	resolved.Assets[0].Asset.Preload[0] = "mutated"
	if plan.Manifest.Entries[0].Elements[0] != "fire" {
		t.Fatal("resolved assets alias the input plan")
	}
	if _, err := ResolvePresentationPlan(plan, testVisualResolver{revision: "r2", digest: "d1"}); !errors.Is(err, ErrVisualCatalogMismatch) {
		t.Fatalf("catalog mismatch = %v", err)
	}
	plan.Effects[0].VisualIndex = 9
	if _, err := ResolvePresentationPlan(plan, testVisualResolver{revision: "r1", digest: "d1"}); !errors.Is(err, ErrVisualMountInvalid) {
		t.Fatalf("invalid mount = %v", err)
	}
}
