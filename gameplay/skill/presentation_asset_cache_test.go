package skill

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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

// VisualPlanCache 的共享加载不能让一个调用者的取消决定别的调用者的结果：创建者取消后，同一 plan / 同一 asset 上
// ctx 仍有效的等待者照常拿到加载结果；真实的加载失败与等待者自己的取消照常报告。

type gatedVisualResolver struct{}

func (gatedVisualResolver) CatalogIdentity() (string, string) { return "r1", "d1" }
func (gatedVisualResolver) Resolve(visual VisualView) (VisualAsset, error) {
	return VisualAsset{Key: "asset-" + visual.Category, Preload: visual.Elements}, nil
}

// gatedVisualLoader 在每次 Preload 进入时通知测试，然后等放行或 ctx 结束；
// 放行前的调用不会自行返回，顺序完全由测试控制。
type gatedVisualLoader struct {
	entered  chan string
	release  chan struct{}
	failWith error
	mutex    sync.Mutex
	loaded   map[string]int
}

func newGatedVisualLoader() *gatedVisualLoader {
	return &gatedVisualLoader{entered: make(chan string, 8), release: make(chan struct{}), loaded: map[string]int{}}
}

func (loader *gatedVisualLoader) Preload(ctx context.Context, asset VisualAsset) error {
	loader.entered <- asset.Key
	select {
	case <-loader.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if loader.failWith != nil {
		return loader.failWith
	}
	loader.mutex.Lock()
	loader.loaded[asset.Key]++
	loader.mutex.Unlock()
	return nil
}

func (loader *gatedVisualLoader) Unload(asset VisualAsset) error {
	loader.mutex.Lock()
	loader.loaded[asset.Key]--
	loader.mutex.Unlock()
	return nil
}

func gatedPlan(digest, category string) PresentationPlan {
	return PresentationPlan{Identity: ProgramIdentityView{PresentationDigest: digest}, Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Category: category, Theme: "default", Elements: []string{"fire"}}}}, Effects: []PresentationMount{{VisualIndex: 0}}}
}

func waitForEntryRefs(t *testing.T, cache *VisualPlanCache, digest string, refs int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cache.mutex.Lock()
		entry := cache.entries[digest]
		got := 0
		if entry != nil {
			got = entry.refs
		}
		cache.mutex.Unlock()
		if got == refs {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("plan %s refs = %d, want %d", digest, got, refs)
		}
		time.Sleep(time.Millisecond)
	}
}

// giveAssetWaiterTime 给资产层等待者一个有界窗口进入等待：它不改任何可观察状态，测试无法精确同步。
// 等待者晚到时它会成为新的创建者；断言在两种顺序下都成立，不会因时序误报。
func giveAssetWaiterTime() { time.Sleep(20 * time.Millisecond) }

func TestVisualPlanCacheWaiterSurvivesCreatorCancellation(t *testing.T) {
	loader := newGatedVisualLoader()
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: gatedVisualResolver{}, Loader: loader})
	if err != nil {
		t.Fatal(err)
	}
	plan := gatedPlan("plan-1", "impact")
	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	creatorDone := make(chan error, 1)
	go func() {
		lease, err := cache.Acquire(creatorCtx, plan)
		if lease != nil {
			_ = lease.Release()
		}
		creatorDone <- err
	}()
	if key := <-loader.entered; key != "asset-impact" {
		t.Fatalf("first preload = %q", key)
	}
	type result struct {
		lease *VisualPlanLease
		err   error
	}
	waiterDone := make(chan result, 1)
	go func() {
		lease, err := cache.Acquire(context.Background(), plan)
		waiterDone <- result{lease, err}
	}()
	waitForEntryRefs(t, cache, "plan-1", 2)

	cancelCreator()
	if err := <-creatorDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("creator = %v, want its own cancellation", err)
	}
	// 等待者重新加载：放行它发起的那一次 Preload。
	select {
	case <-loader.entered:
	case got := <-waiterDone:
		t.Fatalf("waiter with a live context returned before reloading: lease=%v err=%v", got.lease != nil, got.err)
	case <-time.After(5 * time.Second):
		t.Fatal("waiter neither reloaded nor returned")
	}
	close(loader.release)
	got := <-waiterDone
	if got.err != nil {
		t.Fatalf("waiter with a live context = %v; the creator's cancellation decided its result", got.err)
	}
	if err := got.lease.Release(); err != nil {
		t.Fatal(err)
	}
	loader.mutex.Lock()
	defer loader.mutex.Unlock()
	if loader.loaded["asset-impact"] != 0 {
		t.Fatalf("asset references after release = %d, want 0", loader.loaded["asset-impact"])
	}
}

// 资产层：两个不同 plan 共用一个 asset key，持有 asset 加载的 plan 被取消后，
// 另一个 plan 的等待者重新加载该 asset。
func TestVisualAssetWaiterSurvivesCreatorCancellation(t *testing.T) {
	loader := newGatedVisualLoader()
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: gatedVisualResolver{}, Loader: loader})
	if err != nil {
		t.Fatal(err)
	}
	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	creatorDone := make(chan error, 1)
	go func() {
		_, err := cache.Acquire(creatorCtx, gatedPlan("plan-a", "impact"))
		creatorDone <- err
	}()
	<-loader.entered
	waiterDone := make(chan error, 1)
	var waiterLease *VisualPlanLease
	go func() {
		lease, err := cache.Acquire(context.Background(), gatedPlan("plan-b", "impact"))
		waiterLease = lease
		waiterDone <- err
	}()
	waitForEntryRefs(t, cache, "plan-b", 1)
	giveAssetWaiterTime()
	cancelCreator()
	if err := <-creatorDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("creator = %v", err)
	}
	select {
	case <-loader.entered:
	case err := <-waiterDone:
		t.Fatalf("asset waiter returned before reloading: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("asset waiter neither reloaded nor returned")
	}
	close(loader.release)
	if err := <-waiterDone; err != nil {
		t.Fatalf("asset waiter with a live context = %v", err)
	}
	if err := waiterLease.Release(); err != nil {
		t.Fatal(err)
	}
}

// 控制：Loader 真实失败时等待者照常得到该错误，不重试成功；等待者自己取消
// 时立即返回自己的 ctx 错误。
func TestVisualPlanCacheWaiterSeesRealLoadFailureAndOwnCancellation(t *testing.T) {
	loader := newGatedVisualLoader()
	loader.failWith = errors.New("not installed")
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: gatedVisualResolver{}, Loader: loader})
	if err != nil {
		t.Fatal(err)
	}
	plan := gatedPlan("plan-1", "impact")
	creatorDone := make(chan error, 1)
	go func() {
		_, err := cache.Acquire(context.Background(), plan)
		creatorDone <- err
	}()
	<-loader.entered
	waiterDone := make(chan error, 1)
	go func() {
		_, err := cache.Acquire(context.Background(), plan)
		waiterDone <- err
	}()
	waitForEntryRefs(t, cache, "plan-1", 2)
	cancelledCtx, cancelOwn := context.WithCancel(context.Background())
	ownDone := make(chan error, 1)
	go func() {
		_, err := cache.Acquire(cancelledCtx, plan)
		ownDone <- err
	}()
	waitForEntryRefs(t, cache, "plan-1", 3)
	cancelOwn()
	if err := <-ownDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("self-cancelled waiter = %v", err)
	}
	close(loader.release)
	if err := <-creatorDone; err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("creator = %v, want the loader failure", err)
	}
	if err := <-waiterDone; err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("waiter = %v, want the loader failure", err)
	}
}

// gatedLoader parks the preload of one asset until released, so a second
// Acquire of the same plan is caught waiting on the entry's ready channel.
type gatedLoader struct {
	cacheVisualLoader
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (loader *gatedLoader) Preload(ctx context.Context, asset VisualAsset) error {
	if asset.Key == "fallback" {
		loader.once.Do(func() { close(loader.entered) })
		<-loader.gate
	}
	return loader.cacheVisualLoader.Preload(ctx, asset)
}

// 等待另一个 Acquire 加载中 plan 的 Acquire 从开始等待起就持有引用：第一个持有者 Release 时不能因 refs == 0
// 淘汰条目、卸载资产，而等待者正要把它们交出去；最后一个 Release 之后资产才卸载。
func TestAWaitingAcquireIsNotEvictedByTheFirstHoldersRelease(t *testing.T) {
	loader := &gatedLoader{gate: make(chan struct{}), entered: make(chan struct{})}
	cache, err := NewVisualPlanCache(VisualPlanCacheOptions{Resolver: cacheVisualResolver{}, Trust: TrustedVisualCatalogs{"r1": "d1"}, Loader: loader, RequireTrust: true, MaxIdlePlans: 0})
	if err != nil {
		t.Fatal(err)
	}
	plan := PresentationPlan{Identity: ProgramIdentityView{PresentationDigest: "plan-race"}, Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Category: "impact", Theme: "default", Elements: []string{"fire"}}}}, Effects: []PresentationMount{{VisualIndex: 0}}}

	type result struct {
		lease *VisualPlanLease
		err   error
	}
	first := make(chan result, 1)
	go func() {
		lease, err := cache.Acquire(context.Background(), plan)
		first <- result{lease, err}
	}()
	<-loader.entered // the first Acquire is inside the loader
	second := make(chan result, 1)
	go func() {
		lease, err := cache.Acquire(context.Background(), plan)
		second <- result{lease, err}
	}()
	time.Sleep(50 * time.Millisecond) // let the second Acquire reach the ready wait
	close(loader.gate)

	a := <-first
	if a.err != nil {
		t.Fatal(a.err)
	}
	if err := a.lease.Release(); err != nil {
		t.Fatal(err)
	}
	b := <-second
	if b.err != nil {
		t.Fatalf("the waiting Acquire failed: %v", b.err)
	}
	loader.mutex.Lock()
	unloaded := len(loader.unloaded)
	loader.mutex.Unlock()
	if unloaded != 0 {
		t.Fatalf("the first holder's release unloaded %v while a second lease was being handed out", loader.unloaded)
	}
	if got := b.lease.Plan().Assets[0].Asset.Key; got != "fallback" {
		t.Fatalf("second lease asset = %q", got)
	}
	if err := b.lease.Release(); err != nil {
		t.Fatal(err)
	}
	loader.mutex.Lock()
	defer loader.mutex.Unlock()
	if len(loader.unloaded) != 1 {
		t.Fatalf("after the last release the plan's asset was not unloaded: %v", loader.unloaded)
	}
}
