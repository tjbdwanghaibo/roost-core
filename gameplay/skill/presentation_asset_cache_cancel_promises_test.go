package skill

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// RR-20261005-NC-153：VisualPlanCache 的共享加载不能让一个调用者的取消决定
// 别的调用者的结果。旧实现里第一个 Acquire 用自己的 ctx 跑整个 preload，同一
// plan / 同一 asset 上等待的调用者醒来直接拿创建者的错误——创建者取消后，ctx
// 仍有效的等待者也得到 context.Canceled。

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

// giveAssetWaiterTime 给资产层等待者一个有界窗口进入等待：它不改任何可观察
// 状态，测试无法精确同步。顺序只影响修前能否复现——等待者晚到时它会成为新的
// 创建者，修前修后都成功；断言在两种顺序下都成立，不会因时序误报。
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
