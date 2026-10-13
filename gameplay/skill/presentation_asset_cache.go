package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrVisualCatalogUntrusted = errors.New("skill: visual catalog is not trusted")
	ErrVisualLoaderRequired   = errors.New("skill: visual asset loader is required")
	ErrVisualAssetInUse       = errors.New("skill: visual asset plan is in use")
	ErrVisualDigestCollision  = errors.New("skill: presentation digest maps to different plan content")
	ErrVisualAssetCollision   = errors.New("skill: visual asset key maps to different preload content")
)

// VisualCatalogTrust 核对表现 catalog 的修订与 digest 是否可信。
type VisualCatalogTrust interface {
	VerifyCatalog(revision, digest string) error
}

// TrustedVisualCatalogs 是“修订 → 期望 digest”的可信表。
type TrustedVisualCatalogs map[string]string

// VerifyCatalog 在修订不在表里或 digest 不符时返回 ErrVisualCatalogUntrusted。
func (catalogs TrustedVisualCatalogs) VerifyCatalog(revision, digest string) error {
	if expected, ok := catalogs[revision]; !ok || expected != digest {
		return ErrVisualCatalogUntrusted
	}
	return nil
}

// VisualAssetLoader 由渲染端实现，负责预加载与卸载表现资源。
type VisualAssetLoader interface {
	Preload(context.Context, VisualAsset) error
	Unload(VisualAsset) error
}

// VisualPlanCacheOptions 是 VisualPlanCache 的依赖与上限；PreloadConcurrency 非正时取 4，MaxIdlePlans 为负时取 0。
type VisualPlanCacheOptions struct {
	Resolver           VisualAssetResolver
	Trust              VisualCatalogTrust
	Loader             VisualAssetLoader
	RequireTrust       bool
	PreloadConcurrency int
	MaxIdlePlans       int
}

type visualCacheEntry struct {
	ready           chan struct{}
	resolved        ResolvedPresentationPlan
	err             error
	refs            int
	lastUsed        time.Time
	fingerprint     string
	catalogRevision string
	catalogDigest   string
	assetKeys       []string
	// abandoned 标记一次因为发起者的 context 结束、而不是因为计划内容而失败的加载。自己的 context 仍有效的等待者
	// 会重新加载，而不是继承这次取消。
	abandoned bool
}

type visualAssetEntry struct {
	ready       chan struct{}
	asset       VisualAsset
	err         error
	refs        int
	fingerprint string
	aliasKey    string
	abandoned   bool // 含义同 visualCacheEntry.abandoned
}

// VisualPlanCache 按 presentation digest 缓存已解析并预加载的表现计划，同一计划与同一资源在并发调用方之间
// 只加载一次，按引用计数释放，空闲计划超过 MaxIdlePlans 时淘汰最久未用的。
type VisualPlanCache struct {
	mutex        sync.Mutex
	resolver     VisualAssetResolver
	trust        VisualCatalogTrust
	loader       VisualAssetLoader
	requireTrust bool
	concurrency  int
	maxIdle      int
	entries      map[string]*visualCacheEntry
	assets       map[string]*visualAssetEntry
}

// VisualPlanLease 是对一个已加载计划的引用；用完调用 Release。
type VisualPlanLease struct {
	cache    *VisualPlanCache
	digest   string
	resolved ResolvedPresentationPlan
	released atomic.Bool
}

// NewVisualPlanCache 构造缓存。缺少 Resolver 返回 ErrVisualAssetMissing，缺少 Loader 返回 ErrVisualLoaderRequired，
// 要求可信核对却没有 Trust 时返回 ErrVisualCatalogUntrusted。
func NewVisualPlanCache(options VisualPlanCacheOptions) (*VisualPlanCache, error) {
	if options.Resolver == nil {
		return nil, ErrVisualAssetMissing
	}
	if options.Loader == nil {
		return nil, ErrVisualLoaderRequired
	}
	if options.RequireTrust && options.Trust == nil {
		return nil, ErrVisualCatalogUntrusted
	}
	if options.PreloadConcurrency <= 0 {
		options.PreloadConcurrency = 4
	}
	if options.MaxIdlePlans < 0 {
		options.MaxIdlePlans = 0
	}
	return &VisualPlanCache{resolver: options.Resolver, trust: options.Trust, loader: options.Loader, requireTrust: options.RequireTrust, concurrency: options.PreloadConcurrency, maxIdle: options.MaxIdlePlans, entries: make(map[string]*visualCacheEntry), assets: make(map[string]*visualAssetEntry)}, nil
}

// Acquire 返回计划的租约，所有并发调用方共享一次加载。第一个调用方用自己的 ctx 执行共享加载；若加载只因第一个
// 调用方的 ctx 结束而结束，条目被丢弃，每个 ctx 仍有效的等待者重试（成为下一个加载者），一个调用方的取消不会
// 决定别人的结果。等待者自己的 ctx 取消时仍立即返回。
func (cache *VisualPlanCache) Acquire(ctx context.Context, plan PresentationPlan) (*VisualPlanLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	digest := plan.Identity.PresentationDigest
	if digest == "" {
		return nil, ErrVisualAssetMissing
	}
	fingerprint, err := presentationPlanFingerprint(plan)
	if err != nil {
		return nil, err
	}
	for {
		cache.mutex.Lock()
		entry := cache.entries[digest]
		if entry == nil {
			entry = &visualCacheEntry{ready: make(chan struct{}), refs: 1, fingerprint: fingerprint, catalogRevision: plan.Manifest.CatalogRevision, catalogDigest: plan.Manifest.CatalogDigest}
			cache.entries[digest] = entry
			cache.mutex.Unlock()
			cache.load(ctx, entry, plan)
		} else {
			if entry.fingerprint != fingerprint {
				cache.mutex.Unlock()
				return nil, ErrVisualDigestCollision
			}
			// 在放开缓存锁之前先占引用：这次 acquire 等待 ready 期间，并发的最后一次 Release 不能淘汰 / 卸载这个计划。
			entry.refs++
			cache.mutex.Unlock()
		}
		select {
		case <-entry.ready:
		case <-ctx.Done():
			cache.cancelAcquireReservation(digest, entry)
			return nil, ctx.Err()
		}
		cache.mutex.Lock()
		if entry.err != nil {
			if entry.refs > 0 {
				entry.refs--
			}
			retry := entry.abandoned && ctx.Err() == nil
			err := entry.err
			cache.mutex.Unlock()
			if retry {
				continue
			}
			return nil, err
		}
		entry.lastUsed = time.Now()
		lease := &VisualPlanLease{cache: cache, digest: digest, resolved: cloneResolvedPlan(entry.resolved)}
		cache.mutex.Unlock()
		return lease, nil
	}
}

func (cache *VisualPlanCache) cancelAcquireReservation(digest string, entry *visualCacheEntry) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if entry != nil && entry.refs > 0 {
		entry.refs--
	}
	// 这里不淘汰：条目可能仍在加载。加载完成的空闲条目由正常的租约释放或显式失效处理。
}

func (cache *VisualPlanCache) load(ctx context.Context, entry *visualCacheEntry, plan PresentationPlan) {
	var resolved ResolvedPresentationPlan
	var err error
	if cache.requireTrust || cache.trust != nil {
		if cache.trust == nil {
			err = ErrVisualCatalogUntrusted
		} else {
			err = cache.trust.VerifyCatalog(plan.Manifest.CatalogRevision, plan.Manifest.CatalogDigest)
		}
	}
	if err == nil {
		resolved, err = ResolvePresentationPlan(plan, cache.resolver)
	}
	if err == nil {
		entry.assetKeys, err = cache.preload(ctx, &resolved)
	}
	cache.mutex.Lock()
	entry.resolved, entry.err, entry.lastUsed = resolved, err, time.Now()
	entry.abandoned = err != nil && ctx.Err() != nil
	close(entry.ready)
	if err != nil {
		delete(cache.entries, plan.Identity.PresentationDigest)
	}
	cache.mutex.Unlock()
}

func (cache *VisualPlanCache) preload(ctx context.Context, resolved *ResolvedPresentationPlan) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	semaphore := make(chan struct{}, cache.concurrency)
	errorsChannel := make(chan error, len(resolved.Assets))
	loaded := make(chan string, len(resolved.Assets))
	var wait sync.WaitGroup
	for index := range resolved.Assets {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				errorsChannel <- ctx.Err()
				return
			}
			defer func() { <-semaphore }()
			asset, assetKey, err := cache.acquireAsset(ctx, resolved.Assets[index].Asset)
			if err != nil {
				errorsChannel <- err
				cancel()
				return
			}
			resolved.Assets[index].Asset = asset
			loaded <- assetKey
		}()
	}
	wait.Wait()
	close(errorsChannel)
	close(loaded)
	var first error
	for err := range errorsChannel {
		if first == nil {
			first = err
		}
	}
	if first != nil {
		keys := drainAssetKeys(loaded)
		return nil, errors.Join(first, cache.releaseAssets(keys))
	}
	return drainAssetKeys(loaded), nil
}

func drainAssetKeys(values <-chan string) []string {
	result := make([]string, 0)
	for value := range values {
		result = append(result, value)
	}
	return result
}

// acquireAsset 按资源键共享一次 Preload，方式与 Acquire 共享计划加载相同：发起者的加载因发起者的 context 被放弃
// （它所属计划的 ctx 结束，或兄弟资源失败而取消）时，等待者重试。
func (cache *VisualPlanCache) acquireAsset(ctx context.Context, requested VisualAsset) (VisualAsset, string, error) {
	fingerprint, err := visualAssetFingerprint(requested)
	if err != nil {
		return VisualAsset{}, "", err
	}
	for {
		asset, key, retry, err := cache.acquireAssetOnce(ctx, requested, fingerprint)
		if !retry {
			return asset, key, err
		}
	}
}

func (cache *VisualPlanCache) acquireAssetOnce(ctx context.Context, requested VisualAsset, fingerprint string) (VisualAsset, string, bool, error) {
	key := requested.Key
	cache.mutex.Lock()
	entry := cache.assets[key]
	if entry != nil && entry.fingerprint != fingerprint {
		cache.mutex.Unlock()
		return VisualAsset{}, "", false, fmt.Errorf("%w: %s", ErrVisualAssetCollision, key)
	}
	creator := entry == nil
	if creator {
		entry = &visualAssetEntry{ready: make(chan struct{}), fingerprint: fingerprint}
		cache.assets[key] = entry
	}
	cache.mutex.Unlock()

	if creator {
		asset := requested
		loadErr := cache.loader.Preload(ctx, asset)
		if loadErr != nil && asset.FallbackKey != "" {
			if asset.FallbackKey == asset.Key {
				loadErr = fmt.Errorf("preload %q: %w", requested.Key, loadErr)
			} else {
				fallback := cloneVisualAsset(asset)
				fallback.Key, fallback.FallbackKey = asset.FallbackKey, ""
				var aliasKey string
				asset, aliasKey, loadErr = cache.acquireAsset(ctx, fallback)
				entry.aliasKey = aliasKey
			}
		}
		cache.mutex.Lock()
		entry.asset, entry.err = asset, loadErr
		entry.abandoned = loadErr != nil && ctx.Err() != nil
		if loadErr == nil {
			entry.refs = 1
		} else {
			delete(cache.assets, key)
		}
		close(entry.ready)
		cache.mutex.Unlock()
		if loadErr != nil {
			return VisualAsset{}, "", false, fmt.Errorf("preload %q: %w", requested.Key, loadErr)
		}
		return cloneVisualAsset(asset), key, false, nil
	}

	select {
	case <-entry.ready:
	case <-ctx.Done():
		return VisualAsset{}, "", false, ctx.Err()
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if entry.err != nil {
		return VisualAsset{}, "", entry.abandoned && ctx.Err() == nil, entry.err
	}
	entry.refs++
	return cloneVisualAsset(entry.asset), key, false, nil
}

// Plan 返回租约对应的已解析计划副本。
func (lease *VisualPlanLease) Plan() ResolvedPresentationPlan {
	if lease == nil {
		return ResolvedPresentationPlan{}
	}
	return cloneResolvedPlan(lease.resolved)
}

// Release 释放租约，重复调用是空操作。
func (lease *VisualPlanLease) Release() error {
	if lease == nil || !lease.released.CompareAndSwap(false, true) {
		return nil
	}
	return lease.cache.release(lease.digest)
}

func (cache *VisualPlanCache) release(digest string) error {
	cache.mutex.Lock()
	entry := cache.entries[digest]
	if entry == nil {
		cache.mutex.Unlock()
		return nil
	}
	if entry.refs > 0 {
		entry.refs--
	}
	entry.lastUsed = time.Now()
	var evicted []*visualCacheEntry
	for idleCount(cache.entries) > cache.maxIdle {
		key, candidate := oldestIdle(cache.entries)
		if candidate == nil {
			break
		}
		delete(cache.entries, key)
		evicted = append(evicted, candidate)
	}
	cache.mutex.Unlock()
	return cache.releasePlanAssets(evicted)
}

// InvalidateCatalog 淘汰并卸载属于指定 catalog 修订的全部计划。任一相关计划仍在加载或仍被租用时返回
// ErrVisualAssetInUse，且不做任何修改。
func (cache *VisualPlanCache) InvalidateCatalog(revision, digest string) error {
	cache.mutex.Lock()
	var targets []string
	for key, entry := range cache.entries {
		if entry.catalogRevision == revision && entry.catalogDigest == digest {
			select {
			case <-entry.ready:
			default:
				cache.mutex.Unlock()
				return ErrVisualAssetInUse
			}
			if entry.refs != 0 {
				cache.mutex.Unlock()
				return ErrVisualAssetInUse
			}
			targets = append(targets, key)
		}
	}
	// 完整的预检成功之后才开始修改。
	evicted := make([]*visualCacheEntry, 0, len(targets))
	for _, key := range targets {
		evicted = append(evicted, cache.entries[key])
		delete(cache.entries, key)
	}
	cache.mutex.Unlock()
	return cache.releasePlanAssets(evicted)
}

func (cache *VisualPlanCache) releasePlanAssets(entries []*visualCacheEntry) error {
	var result error
	for _, entry := range entries {
		if entry != nil {
			result = errors.Join(result, cache.releaseAssets(entry.assetKeys))
		}
	}
	return result
}

func (cache *VisualPlanCache) releaseAssets(keys []string) error {
	assets := make([]VisualAsset, 0)
	aliases := make([]string, 0)
	cache.mutex.Lock()
	for _, key := range keys {
		entry := cache.assets[key]
		if entry == nil {
			continue
		}
		if entry.refs > 0 {
			entry.refs--
		}
		if entry.refs == 0 {
			delete(cache.assets, key)
			if entry.aliasKey != "" {
				aliases = append(aliases, entry.aliasKey)
			} else {
				assets = append(assets, cloneVisualAsset(entry.asset))
			}
		}
	}
	cache.mutex.Unlock()
	var unloadErrors []error
	for _, asset := range assets {
		if err := cache.loader.Unload(asset); err != nil {
			unloadErrors = append(unloadErrors, fmt.Errorf("unload %q: %w", asset.Key, err))
		}
	}
	var aliasError error
	if len(aliases) > 0 {
		aliasError = cache.releaseAssets(aliases)
	}
	return errors.Join(errors.Join(unloadErrors...), aliasError)
}

func presentationPlanFingerprint(plan PresentationPlan) (string, error) {
	data, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func visualAssetFingerprint(asset VisualAsset) (string, error) {
	data, err := json.Marshal(asset)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func cloneVisualAsset(asset VisualAsset) VisualAsset {
	asset.Preload = append([]string(nil), asset.Preload...)
	return asset
}

func idleCount(values map[string]*visualCacheEntry) int {
	count := 0
	for _, value := range values {
		if value.refs == 0 && value.err == nil {
			count++
		}
	}
	return count
}
func oldestIdle(values map[string]*visualCacheEntry) (string, *visualCacheEntry) {
	var key string
	var result *visualCacheEntry
	for candidateKey, value := range values {
		if value.refs != 0 || value.err != nil {
			continue
		}
		if result == nil || value.lastUsed.Before(result.lastUsed) {
			key, result = candidateKey, value
		}
	}
	return key, result
}
func cloneResolvedPlan(value ResolvedPresentationPlan) ResolvedPresentationPlan {
	value.Plan = clonePresentationPlan(value.Plan)
	value.Assets = append([]ResolvedVisualAsset(nil), value.Assets...)
	for index := range value.Assets {
		value.Assets[index].Visual = cloneVisualView(value.Assets[index].Visual)
		value.Assets[index].Asset.Preload = append([]string(nil), value.Assets[index].Asset.Preload...)
	}
	return value
}
