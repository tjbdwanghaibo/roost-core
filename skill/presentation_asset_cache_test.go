package skill

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type cacheVisualResolver struct{}

func (cacheVisualResolver) CatalogIdentity() (string, string) { return "r1", "d1" }
func (cacheVisualResolver) Resolve(visual VisualView) (VisualAsset, error) {
	return VisualAsset{Key: "missing", FallbackKey: "fallback", Preload: visual.Elements}, nil
}

type cacheVisualLoader struct {
	mutex    sync.Mutex
	loaded   []string
	unloaded []string
}

func (loader *cacheVisualLoader) Preload(_ context.Context, asset VisualAsset) error {
	loader.mutex.Lock()
	defer loader.mutex.Unlock()
	loader.loaded = append(loader.loaded, asset.Key)
	if strings.HasPrefix(asset.Key, "missing") {
		return errors.New("not installed")
	}
	return nil
}

type sharedFallbackResolver struct{}

func (sharedFallbackResolver) CatalogIdentity() (string, string) { return "r1", "d1" }
func (sharedFallbackResolver) Resolve(visual VisualView) (VisualAsset, error) {
	return VisualAsset{Key: "missing-" + visual.Category, FallbackKey: "shared", Preload: visual.Elements}, nil
}
func (loader *cacheVisualLoader) Unload(asset VisualAsset) error {
	loader.mutex.Lock()
	defer loader.mutex.Unlock()
	loader.unloaded = append(loader.unloaded, asset.Key)
	return nil
}

func TestVisualPlanCacheTrustFallbackAndRelease(t *testing.T) {
	loader := &cacheVisualLoader{}
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: cacheVisualResolver{}, Trust: TrustedVisualCatalogs{"r1": "d1"}, Loader: loader, RequireTrust: true, MaxIdlePlans: 0})
	if err != nil {
		t.Fatal(err)
	}
	plan := PresentationPlan{Identity: ProgramIdentityView{PresentationDigest: "plan-1"}, Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Category: "impact", Theme: "default", Elements: []string{"fire"}}}}, Effects: []PresentationMount{{VisualIndex: 0}}}
	lease, err := cache.Acquire(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.Plan().Assets[0].Asset.Key; got != "fallback" {
		t.Fatalf("asset key = %q", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	loader.mutex.Lock()
	defer loader.mutex.Unlock()
	if len(loader.unloaded) != 1 || loader.unloaded[0] != "fallback" {
		t.Fatalf("unloaded = %v", loader.unloaded)
	}
}

func TestVisualPlanCacheRejectsUntrustedCatalog(t *testing.T) {
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: cacheVisualResolver{}, Trust: TrustedVisualCatalogs{"r1": "other"}, Loader: &cacheVisualLoader{}, RequireTrust: true})
	if err != nil {
		t.Fatal(err)
	}
	plan := PresentationPlan{Identity: ProgramIdentityView{PresentationDigest: "plan-1"}, Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1"}}
	if _, err := cache.Acquire(context.Background(), plan); !errors.Is(err, ErrVisualCatalogUntrusted) {
		t.Fatalf("trust error = %v", err)
	}
}

func TestVisualPlanCacheRejectsDigestCollision(t *testing.T) {
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: cacheVisualResolver{}, Loader: &cacheVisualLoader{}, MaxIdlePlans: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan := PresentationPlan{Identity: ProgramIdentityView{PresentationDigest: "same"}, Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1"}}
	lease, err := cache.Acquire(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	changed := plan
	changed.Manifest.CatalogDigest = "different"
	if _, err := cache.Acquire(context.Background(), changed); !errors.Is(err, ErrVisualDigestCollision) {
		t.Fatalf("collision error = %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestVisualPlanCacheSharesAssetUntilLastPlanEviction(t *testing.T) {
	loader := &cacheVisualLoader{}
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: cacheVisualResolver{}, Loader: loader, MaxIdlePlans: 0})
	if err != nil {
		t.Fatal(err)
	}
	base := PresentationPlan{Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Elements: []string{"fire"}}}}}
	first, second := base, base
	first.Identity.PresentationDigest = "first"
	second.Identity.PresentationDigest = "second"
	firstLease, err := cache.Acquire(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	secondLease, err := cache.Acquire(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstLease.Release(); err != nil {
		t.Fatal(err)
	}
	loader.mutex.Lock()
	if len(loader.unloaded) != 0 {
		t.Fatalf("shared asset unloaded early: %v", loader.unloaded)
	}
	loader.mutex.Unlock()
	if err := secondLease.Release(); err != nil {
		t.Fatal(err)
	}
	loader.mutex.Lock()
	defer loader.mutex.Unlock()
	if len(loader.loaded) != 2 || len(loader.unloaded) != 1 || loader.unloaded[0] != "fallback" {
		t.Fatalf("loaded=%v unloaded=%v", loader.loaded, loader.unloaded)
	}
}

func TestVisualPlanCacheSharesResolvedFallbackAcrossDifferentPrimaryKeys(t *testing.T) {
	loader := &cacheVisualLoader{}
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: sharedFallbackResolver{}, Loader: loader, MaxIdlePlans: 0})
	if err != nil {
		t.Fatal(err)
	}
	makePlan := func(digest, category string) PresentationPlan {
		return PresentationPlan{Identity: ProgramIdentityView{PresentationDigest: digest}, Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Category: category, Elements: []string{"common"}}}}}
	}
	first, err := cache.Acquire(context.Background(), makePlan("first-fallback", "a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Acquire(context.Background(), makePlan("second-fallback", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan().Assets[0].Asset.Key != "shared" || second.Plan().Assets[0].Asset.Key != "shared" {
		t.Fatal("fallback was not resolved")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	loader.mutex.Lock()
	defer loader.mutex.Unlock()
	if len(loader.loaded) != 3 || len(loader.unloaded) != 1 || loader.unloaded[0] != "shared" {
		t.Fatalf("loaded=%v unloaded=%v", loader.loaded, loader.unloaded)
	}
}

func TestVisualPlanCacheCatalogInvalidationIsAtomic(t *testing.T) {
	loader := &cacheVisualLoader{}
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: cacheVisualResolver{}, Loader: loader, MaxIdlePlans: 2})
	if err != nil {
		t.Fatal(err)
	}
	base := PresentationPlan{Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Elements: []string{"fire"}}}}}
	idlePlan, activePlan := base, base
	idlePlan.Identity.PresentationDigest = "idle"
	activePlan.Identity.PresentationDigest = "active"
	idle, _ := cache.Acquire(context.Background(), idlePlan)
	active, _ := cache.Acquire(context.Background(), activePlan)
	if err := idle.Release(); err != nil {
		t.Fatal(err)
	}
	if err := cache.InvalidateCatalog("r1", "d1"); !errors.Is(err, ErrVisualAssetInUse) {
		t.Fatalf("invalidate error = %v", err)
	}
	// The failed invalidation must leave the idle plan cached.
	reacquired, err := cache.Acquire(context.Background(), idlePlan)
	if err != nil {
		t.Fatal(err)
	}
	loader.mutex.Lock()
	loaded := len(loader.loaded)
	loader.mutex.Unlock()
	if loaded != 2 {
		t.Fatalf("failed invalidation reloaded asset: %d loads", loaded)
	}
	_ = reacquired.Release()
	_ = active.Release()
	if err := cache.InvalidateCatalog("r1", "d1"); err != nil {
		t.Fatal(err)
	}
}
