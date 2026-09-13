package entity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/cache"
)

var (
	ErrRemoteSnapshotGap            = errors.New("remote snapshot: delta gap")
	ErrRemoteSnapshotEpochMismatch  = errors.New("remote snapshot: epoch mismatch")
	ErrRemoteSnapshotSchemaMismatch = errors.New("remote snapshot: schema mismatch")
	ErrRemoteReadConsistency        = errors.New("remote snapshot: invalid read consistency")
)

type RemoteReadConsistency uint8

const (
	RemoteReadMonotonic RemoteReadConsistency = iota + 1
	RemoteReadCached
	RemoteReadLinearizable
)

type RemoteSnapshotKey struct {
	Tenant   uint32
	EntityID int64
	Kind     EntityKind
	Scope    uint32
	Policy   uint32
}

func (k RemoteSnapshotKey) Valid() bool {
	meta := ResolveEntityID(k.EntityID)
	return meta.FullID == k.EntityID && meta.Kind == k.Kind && k.Kind != EntityKindNone
}

type ImmutableRemoteSnapshot interface {
	RemoteSnapshotSchema() uint32
	RemoteSnapshotSize() int
}

// FrozenRemoteSnapshotPayload owns immutable snapshot bytes. It makes L1 reads
// allocation-free without exposing the cache's backing slice to business code.
type FrozenRemoteSnapshotPayload struct{ data []byte }

func CopyFrozenRemoteSnapshotPayload(data []byte) FrozenRemoteSnapshotPayload {
	return FrozenRemoteSnapshotPayload{data: append([]byte(nil), data...)}
}

// TakeFrozenRemoteSnapshotPayload transfers ownership. The caller must not
// mutate data after this call.
func TakeFrozenRemoteSnapshotPayload(data []byte) FrozenRemoteSnapshotPayload {
	return FrozenRemoteSnapshotPayload{data: data}
}

func (p FrozenRemoteSnapshotPayload) Len() int                   { return len(p.data) }
func (p FrozenRemoteSnapshotPayload) BytesCopy() []byte          { return append([]byte(nil), p.data...) }
func (p FrozenRemoteSnapshotPayload) AppendTo(dst []byte) []byte { return append(dst, p.data...) }

type RemoteSnapshotEnvelope struct {
	Key          RemoteSnapshotKey
	StateVersion uint64
	BaseVersion  uint64
	MarkerEpoch  uint64
	RouteEpoch   uint64
	Schema       uint32
	Codec        uint16
	Checksum     uint64
	Full         bool
	PublishedAt  int64
	ExpiresAt    int64
	Payload      FrozenRemoteSnapshotPayload
}

func (s RemoteSnapshotEnvelope) Clone() RemoteSnapshotEnvelope {
	return s
}

func (s RemoteSnapshotEnvelope) Valid() error {
	if !s.Key.Valid() || s.StateVersion == 0 || s.MarkerEpoch == 0 || s.RouteEpoch == 0 || s.Schema == 0 || s.Payload.Len() == 0 {
		return fmt.Errorf("remote snapshot: invalid envelope")
	}
	if s.Checksum != RemoteSnapshotChecksum(s.Payload.data) {
		return fmt.Errorf("remote snapshot: checksum mismatch")
	}
	if !s.Full && s.BaseVersion == 0 {
		return fmt.Errorf("remote snapshot: delta missing base version")
	}
	return nil
}

func (s RemoteSnapshotEnvelope) Expired(now time.Time) bool {
	return s.ExpiresAt > 0 && now.UnixNano() >= s.ExpiresAt
}

type RemoteSnapshotLoader func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error)

type RemoteSnapshotCacheConfig struct {
	Shards             int
	MaxEntries         int
	MaxBytes           int64
	TTL                time.Duration
	LoadTimeout        time.Duration
	MaxWaiters         int
	MaxConcurrentLoads int
	// TombstoneTTL bounds how long a versioned delete keeps fencing older
	// snapshots for its key (U-0187, RR-20260913-01). Defaults to TTL: a
	// snapshot older than the delete can only arrive late through the same
	// replay window the cache entries themselves live in.
	TombstoneTTL time.Duration
}

// RemoteSnapshotCache is the entity-specific adapter over core/cache. Epoch,
// scope, and minimum-version rules stay here instead of polluting cache.Store.
type RemoteSnapshotCache struct {
	// l2 is kept alongside layered so Publish can ask L2 directly before it
	// writes: ReadThrough treats every L2 error as degradable, which is right
	// for an outage and wrong for a version conflict (RR-20260913-05).
	l2      cache.Store[RemoteSnapshotKey, RemoteSnapshotEnvelope]
	local   *cache.AtomicLocalStore[RemoteSnapshotKey, RemoteSnapshotEnvelope]
	layered *cache.ReadThroughStore[RemoteSnapshotKey, RemoteSnapshotEnvelope]

	waitMu      sync.Mutex
	waiters     map[RemoteSnapshotKey][]remoteVersionWaiter
	waiterCount int
	maxWaiters  int
	loader      RemoteSnapshotLoader
	publishMu   [64]sync.Mutex

	loadMu      sync.Mutex
	loads       map[remoteSnapshotLoadKey]*remoteSnapshotLoadCall
	loadSlots   chan struct{}
	loadTimeout time.Duration

	// tombstones remembers, per key, the newest version a delete was applied
	// at. Delete and Publish for one key serialize on the same publish shard
	// lock, so "delete at v" is ordered against every write of that key
	// (U-0187, RR-20260913-01): a later-arriving snapshot with version <= v
	// is the past and is dropped; a newer one clears the tombstone.
	tombMu       sync.Mutex
	tombstones   map[RemoteSnapshotKey]remoteSnapshotTombstone
	tombstoneTTL time.Duration
}

type remoteSnapshotTombstone struct {
	version uint64
	until   int64 // unix nanos; the tombstone stops fencing after this
}

// remoteSnapshotTombstoneSweepAt is the map size at which an insert also
// sweeps expired tombstones; the bound is time, never a count (a tombstone
// must live through its window, see U-0165's lesson).
const remoteSnapshotTombstoneSweepAt = 1024

type remoteVersionWaiter struct {
	min  uint64
	done chan struct{}
}

type remoteSnapshotLoadKey struct {
	key        RemoteSnapshotKey
	minVersion uint64
}

type remoteSnapshotLoadCall struct {
	done     chan struct{}
	snapshot RemoteSnapshotEnvelope
	ok       bool
	err      error
	waiters  int
}

const (
	defaultRemoteSnapshotEntries           = 64 << 10
	defaultRemoteSnapshotBytes       int64 = 256 << 20
	defaultRemoteSnapshotLoadTimeout       = 3 * time.Second
)

func NewRemoteSnapshotCache(cfg RemoteSnapshotCacheConfig, l2 cache.Store[RemoteSnapshotKey, RemoteSnapshotEnvelope], loader RemoteSnapshotLoader) *RemoteSnapshotCache {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = defaultRemoteSnapshotEntries
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultRemoteSnapshotBytes
	}
	if cfg.LoadTimeout <= 0 {
		cfg.LoadTimeout = defaultRemoteSnapshotLoadTimeout
	}
	if cfg.MaxWaiters <= 0 {
		cfg.MaxWaiters = 256
	}
	if cfg.MaxConcurrentLoads <= 0 {
		cfg.MaxConcurrentLoads = 128
	}
	if cfg.TombstoneTTL <= 0 {
		cfg.TombstoneTTL = cfg.TTL
	}
	if cfg.TombstoneTTL <= 0 {
		cfg.TombstoneTTL = 30 * time.Second
	}
	storeCfg := cache.StoreConfig[RemoteSnapshotKey, RemoteSnapshotEnvelope]{
		KeyOf: func(value RemoteSnapshotEnvelope) RemoteSnapshotKey { return value.Key },
		Stale: func(old, next RemoteSnapshotEnvelope) bool {
			if old.MarkerEpoch != next.MarkerEpoch || old.RouteEpoch != next.RouteEpoch {
				return old.MarkerEpoch > next.MarkerEpoch || old.RouteEpoch > next.RouteEpoch
			}
			return old.StateVersion > next.StateVersion
		},
		ValidateKey:   func(key RemoteSnapshotKey) bool { return key.Valid() },
		ValidateValue: func(value RemoteSnapshotEnvelope) error { return value.Valid() },
		// One rule for every write into L1 — publish, loader fill and L2
		// backfill alike: the same version must be the same value, where
		// "value" includes how its bytes are read (RR-20260913-06).
		Conflict: remoteSnapshotSameVersionConflict,
	}
	local := cache.NewAtomicLocalStore(cache.AtomicLocalConfig[RemoteSnapshotKey, RemoteSnapshotEnvelope]{
		StoreConfig: storeCfg, Shards: cfg.Shards, MaxEntries: cfg.MaxEntries,
		MaxBytes: cfg.MaxBytes, DefaultTTL: cfg.TTL,
		SizeOf: func(value RemoteSnapshotEnvelope) int64 { return int64(value.Payload.Len() + 96) },
	})
	return &RemoteSnapshotCache{
		local: local,
		l2:    l2,
		layered: cache.NewReadThroughStore(local, l2, nil, storeCfg, cache.ReadThroughOptions{
			LocalTTL: cfg.TTL, LoadTimeout: cfg.LoadTimeout, MaxWaitersPerKey: cfg.MaxWaiters,
			// Publish holds a publish shard lock across the L2 write (single
			// point publish is what makes the version CAS meaningful), so the
			// L2 call has to be bounded or an unresponsive Redis pins that
			// shard for every entity hashing to it. IgnoreRemoteError already
			// makes the degraded outcome the right one: skip L2, keep L1.
			RemoteTimeout: cfg.LoadTimeout, IgnoreRemoteError: true,
		}),
		waiters:     make(map[RemoteSnapshotKey][]remoteVersionWaiter),
		maxWaiters:  cfg.MaxWaiters,
		loader:      loader,
		loads:       make(map[remoteSnapshotLoadKey]*remoteSnapshotLoadCall),
		loadSlots:   make(chan struct{}, cfg.MaxConcurrentLoads),
		loadTimeout: cfg.LoadTimeout,

		tombstones:   make(map[RemoteSnapshotKey]remoteSnapshotTombstone),
		tombstoneTTL: cfg.TombstoneTTL,
	}
}

func (c *RemoteSnapshotCache) LoadAuthoritative(ctx context.Context, key RemoteSnapshotKey, consistency RemoteReadConsistency, minVersion uint64) (RemoteSnapshotEnvelope, bool, error) {
	if c == nil || c.loader == nil {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	if !key.Valid() || consistency < RemoteReadMonotonic || consistency > RemoteReadLinearizable {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteReadConsistency
	}
	if ctx == nil {
		ctx = context.Background()
	}
	loadCtx := ctx
	var cancel context.CancelFunc
	if c.loadTimeout > 0 {
		loadCtx, cancel = context.WithTimeout(ctx, c.loadTimeout)
		defer cancel()
	}
	select {
	case c.loadSlots <- struct{}{}:
		defer func() { <-c.loadSlots }()
	case <-loadCtx.Done():
		return RemoteSnapshotEnvelope{}, false, loadCtx.Err()
	}
	snapshot, ok, err := c.loader(loadCtx, key, consistency, minVersion)
	if err != nil || !ok {
		return snapshot, ok, err
	}
	if snapshot.StateVersion < minVersion {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteSnapshotStale
	}
	// Every outward read shares one post-condition: never a snapshot past
	// its own deadline. The authority's raw answer and whatever L1 keeps
	// after Publish both have to pass it (RR-20260913-08 复核: the first fix
	// only covered the cached hit). An expired authoritative answer is a
	// miss, and is not cached — nothing could ever read it.
	now := time.Now()
	if snapshot.Expired(now) {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	if err := c.Publish(loadCtx, snapshot); err != nil {
		return RemoteSnapshotEnvelope{}, false, err
	}
	stored, found, err := c.local.Get(loadCtx, key)
	if err != nil || !found {
		return stored.Clone(), found, err
	}
	if stored.Expired(now) {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	return stored.Clone(), true, nil
}

// remoteSnapshotSameVersionConflict reports that next claims old's version
// but is not the same value. Caller has established the epochs and version
// match.
func remoteSnapshotSameVersionConflict(old, next RemoteSnapshotEnvelope) bool {
	if old.MarkerEpoch != next.MarkerEpoch || old.RouteEpoch != next.RouteEpoch || old.StateVersion != next.StateVersion {
		return false
	}
	return old.Schema != next.Schema || old.Codec != next.Codec || old.Checksum != next.Checksum
}

func (c *RemoteSnapshotCache) Get(ctx context.Context, key RemoteSnapshotKey, consistency RemoteReadConsistency, minVersion uint64) (RemoteSnapshotEnvelope, bool, error) {
	if c == nil || c.layered == nil {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	if !key.Valid() || consistency < RemoteReadMonotonic || consistency > RemoteReadLinearizable {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteReadConsistency
	}
	if consistency == RemoteReadLinearizable {
		return c.LoadAuthoritative(ctx, key, consistency, minVersion)
	}
	snapshot, ok, err := c.layered.Get(ctx, key)
	if err != nil {
		return snapshot, ok, err
	}
	if ok && snapshot.Expired(time.Now()) {
		// The container TTL says how long this machine keeps a copy; the
		// envelope's ExpiresAt says how long the DATA is worth anything.
		// Availability is governed by whichever comes first, so an envelope
		// past its own deadline is a miss no matter how much cache TTL is
		// left — serving it returned a snapshot that declared itself expired
		// (RR-20260913-08). Monotonic then refills from the authority below,
		// which is the contract; Cached answers "not found".
		snapshot, ok = RemoteSnapshotEnvelope{}, false
	}
	if !ok {
		if consistency == RemoteReadMonotonic {
			return c.loadMonotonic(ctx, key, minVersion)
		}
		return snapshot, false, nil
	}
	if consistency == RemoteReadCached || snapshot.StateVersion >= minVersion {
		return snapshot, true, nil
	}
	if consistency == RemoteReadMonotonic {
		return c.loadMonotonic(ctx, key, minVersion)
	}
	return RemoteSnapshotEnvelope{}, false, ErrRemoteSnapshotStale
}

func (c *RemoteSnapshotCache) loadMonotonic(ctx context.Context, key RemoteSnapshotKey, minVersion uint64) (RemoteSnapshotEnvelope, bool, error) {
	if c.loader == nil {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteSnapshotStale
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callKey := remoteSnapshotLoadKey{key: key, minVersion: minVersion}
	c.loadMu.Lock()
	if call := c.loads[callKey]; call != nil {
		if call.waiters >= c.maxWaiters {
			c.loadMu.Unlock()
			return RemoteSnapshotEnvelope{}, false, ErrRemoteOverloaded
		}
		call.waiters++
		c.loadMu.Unlock()
		select {
		case <-call.done:
			return call.snapshot.Clone(), call.ok, call.err
		case <-ctx.Done():
			// Give the slot back. maxWaiters counts who is WAITING, not who
			// has ever waited during this load: a follower that times out and
			// retries used to consume a slot permanently, so a slow authority
			// refused healthy readers with ErrRemoteOverloaded until the first
			// load finished (RR-20260913-04). Only decrement while this is
			// still the current call for the key, or a later generation's
			// count would be charged for a departure that was not its own.
			c.loadMu.Lock()
			if c.loads[callKey] == call {
				call.waiters--
			}
			c.loadMu.Unlock()
			return RemoteSnapshotEnvelope{}, false, ctx.Err()
		}
	}
	call := &remoteSnapshotLoadCall{done: make(chan struct{})}
	c.loads[callKey] = call
	c.loadMu.Unlock()

	call.snapshot, call.ok, call.err = c.LoadAuthoritative(ctx, key, RemoteReadMonotonic, minVersion)
	c.loadMu.Lock()
	delete(c.loads, callKey)
	close(call.done)
	c.loadMu.Unlock()
	return call.snapshot.Clone(), call.ok, call.err
}

func (c *RemoteSnapshotCache) Publish(ctx context.Context, snapshot RemoteSnapshotEnvelope) error {
	if c == nil || c.layered == nil {
		return nil
	}
	snapshot.Checksum = RemoteSnapshotChecksum(snapshot.Payload.data)
	lock := &c.publishMu[remoteSnapshotPublishShard(snapshot.Key)]
	lock.Lock()
	defer lock.Unlock()
	if c.deletedAtOrAfter(snapshot.Key, snapshot.StateVersion, time.Now()) {
		// A delete newer than this snapshot already went through: the
		// snapshot is the past, exactly like losing the version CAS below.
		return nil
	}
	current, ok, err := c.local.Get(ctx, snapshot.Key)
	if err != nil {
		return err
	}
	if ok && current.MarkerEpoch == snapshot.MarkerEpoch && current.RouteEpoch == snapshot.RouteEpoch && current.StateVersion == snapshot.StateVersion {
		if remoteSnapshotSameVersionConflict(current, snapshot) {
			return fmt.Errorf("%w: same snapshot version has different content", ErrRemoteVersionConflict)
		}
		// Same value. Only its expiry metadata can legitimately move, and only
		// forward: a fresher deadline from the authority must replace a copy
		// that has since expired, or an expired L1 entry pins the key at
		// "expired" for that version forever (RR-20260913-08 复核).
		if snapshot.ExpiresAt <= current.ExpiresAt {
			return nil
		}
	}
	if !ok && c.l2 != nil {
		// L1 knows nothing about this key, so the only place a same-version
		// value can already exist is L2. Ask it before writing: a conflict
		// there is a consistency error and must surface, whereas the write
		// path below treats every L2 error as an outage to degrade around.
		// An L2 read error IS an outage and is ignored here on purpose.
		remoteCtx, cancel := context.WithTimeout(ctx, c.loadTimeout)
		stored, held, getErr := c.l2.Get(remoteCtx, snapshot.Key)
		cancel()
		if getErr == nil && held &&
			stored.MarkerEpoch == snapshot.MarkerEpoch && stored.RouteEpoch == snapshot.RouteEpoch && stored.StateVersion == snapshot.StateVersion &&
			remoteSnapshotSameVersionConflict(stored, snapshot) {
			return fmt.Errorf("%w: L2 holds the same snapshot version with different content", ErrRemoteVersionConflict)
		}
	}
	if err := c.layered.Set(ctx, snapshot); err != nil {
		// Losing to a newer snapshot is the intended outcome here, not a
		// failure: single-point publish plus the version predicate means the
		// stored value is already at least as new as this one. Waiters are
		// still notified below — notify only wakes those whose target is
		// <= this version, and a newer stored value satisfies them too.
		if !errors.Is(err, cache.ErrStaleWrite) {
			return err
		}
	}
	c.notify(snapshot.Key, snapshot.StateVersion)
	return nil
}

func remoteSnapshotPublishShard(key RemoteSnapshotKey) uint64 {
	h := uint64(key.EntityID) ^ uint64(key.Tenant)<<32 ^ uint64(key.Kind)<<48
	h ^= uint64(key.Scope)*0x9e3779b185ebca87 ^ uint64(key.Policy)*0xc2b2ae3d27d4eb4f
	return h & 63
}

func (c *RemoteSnapshotCache) ApplyUpdate(ctx context.Context, update RemoteSnapshotRecord) error {
	if c == nil {
		return nil
	}
	if update.Checksum != 0 && RemoteSnapshotChecksum(update.Data) != update.Checksum {
		return fmt.Errorf("remote snapshot: checksum mismatch")
	}
	if update.Full {
		return c.Publish(ctx, RemoteSnapshotEnvelope{
			Key: update.Key, BaseVersion: update.BaseVersion, StateVersion: update.StateVersion,
			MarkerEpoch: update.MarkerEpoch, RouteEpoch: update.RouteEpoch,
			Schema: update.Schema, Codec: update.Codec, Full: true,
			PublishedAt: time.Now().UnixNano(), Payload: CopyFrozenRemoteSnapshotPayload(update.Data),
		})
	}
	current, ok, err := c.local.Get(ctx, update.Key)
	if err != nil {
		return err
	}
	if !ok || current.StateVersion != update.BaseVersion {
		return ErrRemoteSnapshotGap
	}
	if current.MarkerEpoch != update.MarkerEpoch || current.RouteEpoch != update.RouteEpoch {
		return ErrRemoteSnapshotEpochMismatch
	}
	if current.Schema != update.Schema || current.Codec != update.Codec {
		return ErrRemoteSnapshotSchemaMismatch
	}
	data, err := applyRemoteSnapshotDelta(update.Schema, current.Payload.data, update.Data)
	if err != nil {
		return err
	}
	return c.Publish(ctx, RemoteSnapshotEnvelope{
		Key: update.Key, BaseVersion: update.BaseVersion, StateVersion: update.StateVersion,
		MarkerEpoch: update.MarkerEpoch, RouteEpoch: update.RouteEpoch,
		Schema: update.Schema, Codec: update.Codec, Full: false,
		PublishedAt: time.Now().UnixNano(), Payload: TakeFrozenRemoteSnapshotPayload(data),
	})
}

// Delete is the unversioned invalidation primitive: it drops the key from L1
// and L2 and leaves no fence, so a late older snapshot may repopulate it.
// Replication and commit paths must use DeleteAtVersion.
func (c *RemoteSnapshotCache) Delete(ctx context.Context, key RemoteSnapshotKey) error {
	if c == nil || c.layered == nil {
		return nil
	}
	return c.layered.Delete(ctx, key)
}

// DeleteAtVersion applies a delete that happened at version. It is ordered
// against Publish by the publish shard lock (U-0187, RR-20260913-01):
//
//   - a cached snapshot newer than version survives — the delete is the
//     past relative to what L1 holds (delete v2 delivered after full v3);
//   - otherwise the key is dropped and a tombstone at version fences every
//     snapshot with StateVersion <= version for TombstoneTTL (delete v2
//     delivered before full v1 keeps the key deleted).
//
// A snapshot newer than the tombstone clears it on the way in.
func (c *RemoteSnapshotCache) DeleteAtVersion(ctx context.Context, key RemoteSnapshotKey, version uint64) error {
	if c == nil || c.layered == nil {
		return nil
	}
	lock := &c.publishMu[remoteSnapshotPublishShard(key)]
	lock.Lock()
	defer lock.Unlock()
	current, ok, err := c.local.Get(ctx, key)
	if err != nil {
		return err
	}
	if ok && current.StateVersion > version {
		return nil
	}
	c.rememberTombstone(key, version, time.Now())
	return c.layered.Delete(ctx, key)
}

// rememberTombstone records a delete at version; a newer existing tombstone
// is kept. Caller holds the key's publish shard lock.
func (c *RemoteSnapshotCache) rememberTombstone(key RemoteSnapshotKey, version uint64, now time.Time) {
	c.tombMu.Lock()
	defer c.tombMu.Unlock()
	if len(c.tombstones) >= remoteSnapshotTombstoneSweepAt {
		for k, t := range c.tombstones {
			if now.UnixNano() > t.until {
				delete(c.tombstones, k)
			}
		}
	}
	if prev, ok := c.tombstones[key]; ok && prev.version > version && now.UnixNano() <= prev.until {
		return
	}
	c.tombstones[key] = remoteSnapshotTombstone{version: version, until: now.Add(c.tombstoneTTL).UnixNano()}
}

// deletedAtOrAfter reports whether a live tombstone fences a write at
// version. A write newer than the tombstone removes it: the key is alive
// again from that version on. Caller holds the key's publish shard lock.
func (c *RemoteSnapshotCache) deletedAtOrAfter(key RemoteSnapshotKey, version uint64, now time.Time) bool {
	c.tombMu.Lock()
	defer c.tombMu.Unlock()
	t, ok := c.tombstones[key]
	if !ok {
		return false
	}
	if now.UnixNano() > t.until || version > t.version {
		delete(c.tombstones, key)
		return false
	}
	return true
}

func (c *RemoteSnapshotCache) WaitForVersion(ctx context.Context, key RemoteSnapshotKey, minVersion uint64) error {
	if minVersion == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if snapshot, ok, _ := c.local.Get(ctx, key); ok && snapshot.StateVersion >= minVersion {
		return nil
	}
	waiter := remoteVersionWaiter{min: minVersion, done: make(chan struct{})}
	c.waitMu.Lock()
	if c.maxWaiters > 0 && c.waiterCount >= c.maxWaiters {
		c.waitMu.Unlock()
		return ErrRemoteOverloaded
	}
	c.waiters[key] = append(c.waiters[key], waiter)
	c.waiterCount++
	c.waitMu.Unlock()
	if snapshot, ok, _ := c.local.Get(ctx, key); ok && snapshot.StateVersion >= minVersion {
		c.notify(key, snapshot.StateVersion)
	}
	select {
	case <-waiter.done:
		return nil
	case <-ctx.Done():
		c.removeWaiter(key, waiter.done)
		return ctx.Err()
	}
}

func (c *RemoteSnapshotCache) Stats() (cache.AtomicLocalStats, cache.ReadThroughStats) {
	if c == nil {
		return cache.AtomicLocalStats{}, cache.ReadThroughStats{}
	}
	return c.local.Stats(), c.layered.Stats()
}

func (c *RemoteSnapshotCache) notify(key RemoteSnapshotKey, version uint64) {
	c.waitMu.Lock()
	waiters := c.waiters[key]
	remaining := waiters[:0]
	for _, waiter := range waiters {
		if version >= waiter.min {
			close(waiter.done)
			c.waiterCount--
		} else {
			remaining = append(remaining, waiter)
		}
	}
	if len(remaining) == 0 {
		delete(c.waiters, key)
	} else {
		c.waiters[key] = remaining
	}
	c.waitMu.Unlock()
}

func (c *RemoteSnapshotCache) removeWaiter(key RemoteSnapshotKey, done chan struct{}) {
	c.waitMu.Lock()
	waiters := c.waiters[key]
	for i := range waiters {
		if waiters[i].done == done {
			waiters = append(waiters[:i], waiters[i+1:]...)
			c.waiterCount--
			break
		}
	}
	if len(waiters) == 0 {
		delete(c.waiters, key)
	} else {
		c.waiters[key] = waiters
	}
	c.waitMu.Unlock()
}
