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

type VisualCatalogTrust interface {
	VerifyCatalog(revision, digest string) error
}

type TrustedVisualCatalogs map[string]string

func (catalogs TrustedVisualCatalogs) VerifyCatalog(revision, digest string) error {
	if expected, ok := catalogs[revision]; !ok || expected != digest {
		return ErrVisualCatalogUntrusted
	}
	return nil
}

type VisualAssetLoader interface {
	Preload(context.Context, VisualAsset) error
	Unload(VisualAsset) error
}

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
}

type visualAssetEntry struct {
	ready       chan struct{}
	asset       VisualAsset
	err         error
	refs        int
	fingerprint string
	aliasKey    string
}

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

type VisualPlanLease struct {
	cache    *VisualPlanCache
	digest   string
	resolved ResolvedPresentationPlan
	released atomic.Bool
}

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
		// Reserve before dropping the cache lock. A concurrent final Release
		// cannot evict/unload the plan while this acquire waits on ready.
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
	defer cache.mutex.Unlock()
	if entry.err != nil {
		if entry.refs > 0 {
			entry.refs--
		}
		return nil, entry.err
	}
	entry.lastUsed = time.Now()
	return &VisualPlanLease{cache: cache, digest: digest, resolved: cloneResolvedPlan(entry.resolved)}, nil
}

func (cache *VisualPlanCache) cancelAcquireReservation(digest string, entry *visualCacheEntry) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if entry != nil && entry.refs > 0 {
		entry.refs--
	}
	// Do not evict here: the entry may still be loading. A completed idle
	// entry is handled by normal lease release or explicit invalidation.
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

func (cache *VisualPlanCache) acquireAsset(ctx context.Context, requested VisualAsset) (VisualAsset, string, error) {
	fingerprint, err := visualAssetFingerprint(requested)
	if err != nil {
		return VisualAsset{}, "", err
	}
	key := requested.Key
	cache.mutex.Lock()
	entry := cache.assets[key]
	if entry != nil && entry.fingerprint != fingerprint {
		cache.mutex.Unlock()
		return VisualAsset{}, "", fmt.Errorf("%w: %s", ErrVisualAssetCollision, key)
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
		if loadErr == nil {
			entry.refs = 1
		} else {
			delete(cache.assets, key)
		}
		close(entry.ready)
		cache.mutex.Unlock()
		if loadErr != nil {
			return VisualAsset{}, "", fmt.Errorf("preload %q: %w", requested.Key, loadErr)
		}
		return cloneVisualAsset(asset), key, nil
	}

	select {
	case <-entry.ready:
	case <-ctx.Done():
		return VisualAsset{}, "", ctx.Err()
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if entry.err != nil {
		return VisualAsset{}, "", entry.err
	}
	entry.refs++
	return cloneVisualAsset(entry.asset), key, nil
}

func (lease *VisualPlanLease) Plan() ResolvedPresentationPlan {
	if lease == nil {
		return ResolvedPresentationPlan{}
	}
	return cloneResolvedPlan(lease.resolved)
}
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
	// Mutation begins only after the complete preflight succeeds.
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
