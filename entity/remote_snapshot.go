package entity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	Checksum     RemoteChecksum
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
	// TombstoneTTL 是 L1 里删除标记的存活时间（U-0187、RR-20260913-01）。缺省取 TTL：比删除更旧的快照只会
	// 经同一个重放窗口迟到。共享 L2 的墓碑另由 L2 TTL 约束，那才是跨节点的删除水位（B2）。
	TombstoneTTL time.Duration
	// MaxStaleness 是非线性读（Cached / Monotonic）能交出的 L1 条目距最近一次确认的最长时间
	// （kit：remote_entity.cached_max_staleness，B2）。超过它、或从未确认的条目先重新确认（读 L2，
	// 必要时修复 L2 或回源权威），确认不了就不交出。零值取 TTL；TTL 也为零时取 30s。
	MaxStaleness time.Duration
	// Now 是缓存的时钟（确认时刻、陈旧上限、L1 过期都用它）。nil 用 time.Now；只为测试控制时间。
	Now func() time.Time
}

// RemoteSnapshotCache 是 Remote 快照的进程内缓存。B2（维护者决定 2026-10-05，方案
// docs/feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md）之后它的契约是：
//
//   - 共享 L2 是快照水位（已知最新版本 / 是否已删除）的唯一权威，L1 只是它的有界副本。
//   - 一个 key 的全部 L1 写入——owner 发布、复制消息、权威加载回填、L2 回填、删除、修复——都在该 key 的
//     publish 分片锁下经 admitLocked：先在 L2 上以版本 CAS（或带版本删除）落地，L1 只记下 L2 接受或
//     L2 已持有的值，带上确认时刻 confirmedAt。L2 拒绝时 L1 改记 L2 当前的值。
//   - L2 回答不了（断网、结果未知）时降级：L1 照记，但 confirmedAt = 0（未确认）。权威加载的结果例外，
//     记为在加载开始时确认。
//   - 非线性读只交出 now − confirmedAt ≤ MaxStaleness 的条目；其余先 refresh 重新确认。
//
// 删除在 L1 里是一个条目（删除标记），与快照用同一条 remoteSnapshotEntryStale 规则排序。
// 之前分散的本机墓碑侧表、cache.StoreConfig.Superseded 钩子、ReadThrough 的 FatalRemoteError 分类与
// L1 冷时的 L2 预查都已删除（它们各自补过 RR-20260913-01/05/06/08、NC-130 的一个入口）。
//
// 没有共享 L2 的装配（测试、单进程）里 L1 自己就是水位：写入直接算确认。
type RemoteSnapshotCache struct {
	l2 cache.Store[RemoteSnapshotKey, RemoteSnapshotEnvelope]
	l1 *cache.AtomicLocalStore[RemoteSnapshotKey, remoteSnapshotEntry]

	ttl          time.Duration
	tombstoneTTL time.Duration
	maxStaleness time.Duration
	now          func() time.Time

	waitMu      sync.Mutex
	waiters     map[RemoteSnapshotKey][]remoteVersionWaiter
	waiterCount int
	maxWaiters  int
	loader      RemoteSnapshotLoader
	// publishMu 串行化同一 key 的全部 L1 写入与它们的 L2 调用（见类型注释）。
	publishMu [64]sync.Mutex

	loadMu      sync.Mutex
	loads       map[remoteSnapshotLoadKey]*remoteSnapshotLoadCall
	loadSlots   chan struct{}
	loadTimeout time.Duration

	authorityLoads atomic.Uint64
	coalesced      atomic.Uint64
	loadErrors     atomic.Uint64
	remoteErrors   atomic.Uint64
}

// remoteSnapshotEntry 是 L1 里的一个条目：一份快照，或一个删除标记（deleted，snapshot 只有 Key 与
// StateVersion 有意义）。confirmedAt 是 L2 或权威最近一次担保“没有更新的版本或删除”的时刻（Unix 纳秒，
// 取调用开始前的本机时间，保守）；0 表示从未确认。
type remoteSnapshotEntry struct {
	snapshot    RemoteSnapshotEnvelope
	deleted     bool
	confirmedAt int64
}

func remoteSnapshotDeleteMarker(key RemoteSnapshotKey, version uint64) remoteSnapshotEntry {
	return remoteSnapshotEntry{snapshot: RemoteSnapshotEnvelope{Key: key, StateVersion: version}, deleted: true}
}

type remoteVersionWaiter struct {
	min  uint64
	done chan struct{}
}

type remoteSnapshotLoadKey struct {
	key        RemoteSnapshotKey
	minVersion uint64
	// refresh 区分“重新确认 / L2 回填”与按最低版本的权威加载，两者各自合并。
	refresh bool
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
	defaultRemoteSnapshotStaleness         = 30 * time.Second
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
		cfg.TombstoneTTL = defaultRemoteSnapshotStaleness
	}
	if cfg.MaxStaleness <= 0 {
		cfg.MaxStaleness = cfg.TTL
	}
	if cfg.MaxStaleness <= 0 {
		cfg.MaxStaleness = defaultRemoteSnapshotStaleness
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	storeCfg := cache.StoreConfig[RemoteSnapshotKey, remoteSnapshotEntry]{
		KeyOf:       func(entry remoteSnapshotEntry) RemoteSnapshotKey { return entry.snapshot.Key },
		Stale:       remoteSnapshotEntryStale,
		ValidateKey: func(key RemoteSnapshotKey) bool { return key.Valid() },
		ValidateValue: func(entry remoteSnapshotEntry) error {
			if entry.deleted {
				if !entry.snapshot.Key.Valid() {
					return fmt.Errorf("remote snapshot: invalid delete marker")
				}
				return nil
			}
			return entry.snapshot.Valid()
		},
		// One rule for every write into L1: the same version must be the same value, where "value"
		// includes how its bytes are read (RR-20260913-06).
		Conflict: func(old, next remoteSnapshotEntry) bool {
			return !old.deleted && !next.deleted && remoteSnapshotSameVersionConflict(old.snapshot, next.snapshot)
		},
	}
	l1 := cache.NewAtomicLocalStore(cache.AtomicLocalConfig[RemoteSnapshotKey, remoteSnapshotEntry]{
		StoreConfig: storeCfg, Shards: cfg.Shards, MaxEntries: cfg.MaxEntries,
		MaxBytes: cfg.MaxBytes, DefaultTTL: cfg.TTL, Now: cfg.Now,
		SizeOf: func(entry remoteSnapshotEntry) int64 { return int64(entry.snapshot.Payload.Len() + 96) },
	})
	return &RemoteSnapshotCache{
		l2: l2, l1: l1,
		ttl: cfg.TTL, tombstoneTTL: cfg.TombstoneTTL, maxStaleness: cfg.MaxStaleness, now: cfg.Now,
		waiters: make(map[RemoteSnapshotKey][]remoteVersionWaiter), maxWaiters: cfg.MaxWaiters,
		loader:    loader,
		loads:     make(map[remoteSnapshotLoadKey]*remoteSnapshotLoadCall),
		loadSlots: make(chan struct{}, cfg.MaxConcurrentLoads), loadTimeout: cfg.LoadTimeout,
	}
}

// RemoteSnapshotVersionedDeleter is the L2 capability behind DeleteAtVersion: delete only if the stored
// snapshot is not newer than version, compared atomically on the L2 side, and keep the delete's version
// as a tombstone that refuses later writes not newer than it (cache.ErrStaleWrite). A stored snapshot
// newer than version is reported as cache.ErrStaleWrite too: the delete is the past (B2). An L2 without
// it is deleted unconditionally, which is the pre-U-0187 behaviour for that layer.
type RemoteSnapshotVersionedDeleter interface {
	DeleteAtVersion(ctx context.Context, key RemoteSnapshotKey, version uint64) error
}

func (c *RemoteSnapshotCache) nowNanos() int64 { return c.now().UnixNano() }

// fresh 报告条目能否不经重新确认直接服务非线性读。
func (c *RemoteSnapshotCache) fresh(entry remoteSnapshotEntry, now int64) bool {
	return entry.confirmedAt != 0 && now-entry.confirmedAt <= c.maxStaleness.Nanoseconds()
}

func (c *RemoteSnapshotCache) l1Entry(ctx context.Context, key RemoteSnapshotKey) (remoteSnapshotEntry, bool) {
	entry, ok, err := c.l1.Get(ctx, key)
	return entry, ok && err == nil
}

// l1Snapshot 返回 L1 里的快照（不论是否已确认）；删除标记与空都是没有。
func (c *RemoteSnapshotCache) l1Snapshot(ctx context.Context, key RemoteSnapshotKey) (RemoteSnapshotEnvelope, bool, error) {
	entry, ok := c.l1Entry(ctx, key)
	if !ok || entry.deleted {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	return entry.snapshot, true, nil
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
	// 权威的答案反映加载开始之后的某个时刻：以开始时刻作确认时刻（保守）。
	loadStart := c.nowNanos()
	c.authorityLoads.Add(1)
	snapshot, ok, err := c.loader(loadCtx, key, consistency, minVersion)
	if err != nil {
		c.loadErrors.Add(1)
		return snapshot, ok, err
	}
	if !ok {
		return snapshot, ok, nil
	}
	// RR-20261005-NC-35：加载回调也属于身份边界，必须在任何缓存写入前
	// 绑定完整请求键；否则会写入别的视图，再把请求键的旧值误当成功返回。
	if snapshot.Key != key {
		return RemoteSnapshotEnvelope{}, false, fmt.Errorf("remote snapshot: authoritative result key does not match requested key")
	}
	if snapshot.StateVersion < minVersion {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteSnapshotStale
	}
	// Every outward read shares one post-condition: never a snapshot past
	// its own deadline. The authority's raw answer and whatever L1 keeps
	// after the write both have to pass it (RR-20260913-08 复核). An expired
	// authoritative answer is a miss, and is not cached.
	if snapshot.Expired(c.now()) {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	snapshot.Checksum = RemoteSnapshotChecksum(snapshot.Payload.data)
	lock := &c.publishMu[remoteSnapshotPublishShard(key)]
	lock.Lock()
	err = c.publishLocked(loadCtx, snapshot, loadStart)
	lock.Unlock()
	if err != nil {
		return RemoteSnapshotEnvelope{}, false, err
	}
	c.notify(key, snapshot.StateVersion)
	stored, found, err := c.l1Snapshot(loadCtx, key)
	if err != nil || !found {
		return stored.Clone(), found, err
	}
	// RR-20260913-08 残余：写入可能等待 L2，不能复用写入前的时间。
	if stored.Expired(c.now()) {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	// RR-20261005-NC-36：epoch 准入或并发发布可能保留另一份 L1；
	// 最低版本承诺约束的是最终返回值，不能只检查权威的原始结果。
	if stored.StateVersion < minVersion {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteSnapshotStale
	}
	return stored.Clone(), true, nil
}

// remoteSnapshotStale reports that next is older than old and must not
// replace it: a newer marker or route epoch wins regardless of version (a
// mixed pair is refused both ways), within one epoch the higher version wins.
// L1 admission and the L2 CAS script apply the same rule.
func remoteSnapshotStale(old, next RemoteSnapshotEnvelope) bool {
	if old.MarkerEpoch != next.MarkerEpoch || old.RouteEpoch != next.RouteEpoch {
		return old.MarkerEpoch > next.MarkerEpoch || old.RouteEpoch > next.RouteEpoch
	}
	return old.StateVersion > next.StateVersion
}

// remoteSnapshotEntryStale 是 L1 唯一的新旧判定，与 L2 的两个脚本一致：快照之间按 remoteSnapshotStale；
// 删除只按版本排序——删除标记挡住不新于它的快照，比快照旧的删除是过去（同版本时删除胜），更旧的删除
// 不降低更新的删除标记。
func remoteSnapshotEntryStale(old, next remoteSnapshotEntry) bool {
	switch {
	case old.deleted && next.deleted:
		return next.snapshot.StateVersion < old.snapshot.StateVersion
	case old.deleted:
		return next.snapshot.StateVersion <= old.snapshot.StateVersion
	case next.deleted:
		return old.snapshot.StateVersion > next.snapshot.StateVersion
	default:
		return remoteSnapshotStale(old.snapshot, next.snapshot)
	}
}

// remoteSnapshotSameVersionConflict reports that next claims old's version
// but is not the same value. Caller has established the epochs and version
// match.
func remoteSnapshotSameVersionConflict(old, next RemoteSnapshotEnvelope) bool {
	if !remoteSnapshotSameVersion(old, next) {
		return false
	}
	return old.Schema != next.Schema || old.Codec != next.Codec || old.Checksum != next.Checksum
}

func remoteSnapshotSameVersion(a, b RemoteSnapshotEnvelope) bool {
	return a.MarkerEpoch == b.MarkerEpoch && a.RouteEpoch == b.RouteEpoch && a.StateVersion == b.StateVersion
}

func (c *RemoteSnapshotCache) Get(ctx context.Context, key RemoteSnapshotKey, consistency RemoteReadConsistency, minVersion uint64) (RemoteSnapshotEnvelope, bool, error) {
	if c == nil || c.l1 == nil {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	if !key.Valid() || consistency < RemoteReadMonotonic || consistency > RemoteReadLinearizable {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteReadConsistency
	}
	if consistency == RemoteReadLinearizable {
		return c.LoadAuthoritative(ctx, key, consistency, minVersion)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot, ok, err := c.readConfirmed(ctx, key)
	if err != nil {
		return RemoteSnapshotEnvelope{}, false, err
	}
	if ok && (consistency == RemoteReadCached || snapshot.StateVersion >= minVersion) {
		return snapshot, true, nil
	}
	if consistency == RemoteReadMonotonic {
		return c.loadMonotonic(ctx, key, minVersion)
	}
	return RemoteSnapshotEnvelope{}, false, nil
}

// readConfirmed 是非线性读看到的 L1：条目在陈旧上限内已确认就直接用（不碰 L2，热路径只有一次 L1 读）；
// 否则经 refresh 重新确认，同一 key 的并发读合并成一次。
func (c *RemoteSnapshotCache) readConfirmed(ctx context.Context, key RemoteSnapshotKey) (RemoteSnapshotEnvelope, bool, error) {
	if entry, ok := c.l1Entry(ctx, key); ok {
		// 热路径：一次 L1 读、一次读时钟。
		if now := c.nowNanos(); c.fresh(entry, now) {
			return serveAt(entry, now)
		}
	}
	return c.coalesce(ctx, remoteSnapshotLoadKey{key: key, refresh: true}, func(ctx context.Context) (RemoteSnapshotEnvelope, bool, error) {
		return c.refresh(ctx, key)
	})
}

// serve 把一个可服务的条目交出去。删除标记是“未找到”。The container TTL says how long this machine
// keeps a copy; the envelope's ExpiresAt says how long the DATA is worth anything — an envelope past its
// own deadline is a miss no matter how much cache TTL is left (RR-20260913-08).
func (c *RemoteSnapshotCache) serve(entry remoteSnapshotEntry) (RemoteSnapshotEnvelope, bool, error) {
	return serveAt(entry, c.nowNanos())
}

func serveAt(entry remoteSnapshotEntry, now int64) (RemoteSnapshotEnvelope, bool, error) {
	if entry.deleted || (entry.snapshot.ExpiresAt > 0 && now >= entry.snapshot.ExpiresAt) {
		return RemoteSnapshotEnvelope{}, false, nil
	}
	return entry.snapshot.Clone(), true, nil
}

// refresh 重新确认一个 key（L1 没有它、未确认或超过陈旧上限）：
//
//   - 先读 L2（锁外，一次 HGET），再在 publish 分片锁下与 L1 当前值比较：L1 已被别人确认就直接用；
//     L2 与 L1 同值改记确认时刻；L2 更新就改记 L2 的值；L1 更新（L2 写丢了）就把 L1 这份重新写进 L2
//     （版本化 CAS 与带版本删除重发都幂等）。
//   - L1 没有条目而 L2 也没有值或读不到：未找到——Cached 不因 miss 读权威（与之前相同），Monotonic 由
//     Get 另行回源。
//   - L1 有快照而 L2 读不到、或已没有值（过期或墓碑）：回源读权威；权威说不存在就删掉加载开始之前确认的
//     L1 快照。都失败时返回错误，不交出未确认或超过上限的值。
func (c *RemoteSnapshotCache) refresh(ctx context.Context, key RemoteSnapshotKey) (RemoteSnapshotEnvelope, bool, error) {
	started := c.nowNanos()
	var stored RemoteSnapshotEnvelope
	var held bool
	var getErr error
	if c.l2 != nil {
		remoteCtx, cancel := context.WithTimeout(ctx, c.loadTimeout)
		stored, held, getErr = c.l2.Get(remoteCtx, key)
		cancel()
		if getErr != nil {
			c.remoteErrors.Add(1)
			held = false
		}
	}
	lock := &c.publishMu[remoteSnapshotPublishShard(key)]
	lock.Lock()
	current, hasCurrent := c.l1Entry(ctx, key)
	if hasCurrent && c.fresh(current, c.nowNanos()) {
		lock.Unlock()
		return c.serve(current)
	}
	if held {
		fromL2 := remoteSnapshotEntry{snapshot: stored, confirmedAt: started}
		switch {
		case !hasCurrent:
			err := c.setL1Locked(ctx, fromL2)
			lock.Unlock()
			if err != nil {
				return RemoteSnapshotEnvelope{}, false, err
			}
			return c.serve(fromL2)
		case !current.deleted && remoteSnapshotSameVersion(current.snapshot, stored):
			if remoteSnapshotSameVersionConflict(current.snapshot, stored) {
				lock.Unlock()
				return RemoteSnapshotEnvelope{}, false, fmt.Errorf("%w: L2 holds the same snapshot version with different content", ErrRemoteVersionConflict)
			}
			current.confirmedAt = started
			err := c.setL1Locked(ctx, current)
			lock.Unlock()
			if err != nil {
				return RemoteSnapshotEnvelope{}, false, err
			}
			return c.serve(current)
		case remoteSnapshotEntryStale(current, fromL2):
			// L1 比 L2 新：L1 这份的 L2 写入丢了或结果未知。补进 L2 再看结果。
			current.confirmedAt = 0
			err := c.admitLocked(ctx, current, 0)
			repaired, ok := c.l1Entry(ctx, key)
			lock.Unlock()
			if err != nil {
				return RemoteSnapshotEnvelope{}, false, err
			}
			if ok && c.fresh(repaired, c.nowNanos()) {
				return c.serve(repaired)
			}
			if ok && repaired.deleted {
				return RemoteSnapshotEnvelope{}, false, nil // 本机知道更新的删除
			}
			return c.loadForRefresh(ctx, key)
		default:
			err := c.setL1Locked(ctx, fromL2)
			lock.Unlock()
			if err != nil {
				return RemoteSnapshotEnvelope{}, false, err
			}
			return c.serve(fromL2)
		}
	}
	switch {
	case !hasCurrent:
		lock.Unlock()
		return RemoteSnapshotEnvelope{}, false, nil
	case current.deleted:
		if getErr == nil {
			// L2 没有活值，与删除一致：改记确认时刻。
			current.confirmedAt = started
			_ = c.setL1Locked(ctx, current)
		}
		lock.Unlock()
		return RemoteSnapshotEnvelope{}, false, nil
	default:
		// L2 担保不了这份快照（读不到、已过期或是墓碑；没有共享 L2 时 L1 自己就是水位）：问权威。
		lock.Unlock()
		return c.loadForRefresh(ctx, key)
	}
}

// loadForRefresh 是 refresh 的回源：L2 担保不了 L1 里的快照时读权威。
func (c *RemoteSnapshotCache) loadForRefresh(ctx context.Context, key RemoteSnapshotKey) (RemoteSnapshotEnvelope, bool, error) {
	if c.loader == nil {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteSnapshotStale
	}
	loadStart := c.nowNanos()
	snapshot, found, err := c.LoadAuthoritative(ctx, key, RemoteReadMonotonic, 0)
	if err != nil || found {
		return snapshot, found, err
	}
	// 权威说不存在（或已过期）：加载开始之前确认的 L1 快照都不能再交出。
	lock := &c.publishMu[remoteSnapshotPublishShard(key)]
	lock.Lock()
	if current, ok := c.l1Entry(ctx, key); ok && !current.deleted && current.confirmedAt < loadStart {
		_ = c.l1.Delete(ctx, key)
	}
	lock.Unlock()
	return RemoteSnapshotEnvelope{}, false, nil
}

func (c *RemoteSnapshotCache) loadMonotonic(ctx context.Context, key RemoteSnapshotKey, minVersion uint64) (RemoteSnapshotEnvelope, bool, error) {
	if c.loader == nil {
		return RemoteSnapshotEnvelope{}, false, ErrRemoteSnapshotStale
	}
	return c.coalesce(ctx, remoteSnapshotLoadKey{key: key, minVersion: minVersion}, func(ctx context.Context) (RemoteSnapshotEnvelope, bool, error) {
		return c.LoadAuthoritative(ctx, key, RemoteReadMonotonic, minVersion)
	})
}

// coalesce 让同一 callKey 的并发调用只执行一次 fn，其余等待它的结果或自己的 ctx。
func (c *RemoteSnapshotCache) coalesce(ctx context.Context, callKey remoteSnapshotLoadKey, fn func(context.Context) (RemoteSnapshotEnvelope, bool, error)) (RemoteSnapshotEnvelope, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.loadMu.Lock()
	if call := c.loads[callKey]; call != nil {
		if call.waiters >= c.maxWaiters {
			c.loadMu.Unlock()
			return RemoteSnapshotEnvelope{}, false, ErrRemoteOverloaded
		}
		call.waiters++
		c.loadMu.Unlock()
		c.coalesced.Add(1)
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

	call.snapshot, call.ok, call.err = fn(ctx)
	c.loadMu.Lock()
	delete(c.loads, callKey)
	close(call.done)
	c.loadMu.Unlock()
	return call.snapshot.Clone(), call.ok, call.err
}

// Publish 记下一份快照（owner 提交后、复制消息、增量合成）。输给更新的快照或删除是预期结果，不是失败。
func (c *RemoteSnapshotCache) Publish(ctx context.Context, snapshot RemoteSnapshotEnvelope) error {
	if c == nil || c.l1 == nil {
		return nil
	}
	snapshot.Checksum = RemoteSnapshotChecksum(snapshot.Payload.data)
	lock := &c.publishMu[remoteSnapshotPublishShard(snapshot.Key)]
	lock.Lock()
	err := c.publishLocked(ctx, snapshot, 0)
	lock.Unlock()
	if err != nil {
		return err
	}
	// Waiters are notified even when the snapshot lost: notify only wakes those whose target is
	// <= this version, and a newer stored value satisfies them too.
	c.notify(snapshot.Key, snapshot.StateVersion)
	return nil
}

// publishLocked 是快照写入的前置判断，之后交给 admitLocked。authoritativeAt 非零表示这份来自权威
// 加载（开始时刻）。调用方持有 key 的 publish 分片锁。
func (c *RemoteSnapshotCache) publishLocked(ctx context.Context, snapshot RemoteSnapshotEnvelope, authoritativeAt int64) error {
	next := remoteSnapshotEntry{snapshot: snapshot}
	if current, ok := c.l1Entry(ctx, snapshot.Key); ok {
		switch {
		case !current.deleted && remoteSnapshotSameVersion(current.snapshot, snapshot):
			if remoteSnapshotSameVersionConflict(current.snapshot, snapshot) {
				return fmt.Errorf("%w: same snapshot version has different content", ErrRemoteVersionConflict)
			}
			// 同一份值：只有有效期（只能向后，RR-20260913-08 复核）与确认状态可能需要更新。已确认、
			// 有效期不更晚、也不是更晚的权威答案时什么都不做——不为重复消息再写一次 L2。
			if snapshot.ExpiresAt <= current.snapshot.ExpiresAt && current.confirmedAt != 0 &&
				(authoritativeAt == 0 || current.confirmedAt >= authoritativeAt) {
				return nil
			}
			if snapshot.ExpiresAt < current.snapshot.ExpiresAt {
				next.snapshot.ExpiresAt = current.snapshot.ExpiresAt
			}
		case current.confirmedAt != 0 && remoteSnapshotEntryStale(current, next):
			// L1 已确认持有更新的快照或不旧于它的删除：这份是过去。不写 L2——L2 若已过期，写进去就是
			// 让旧值复活（N05 O5）。
			return nil
		}
	}
	return c.admitLocked(ctx, next, authoritativeAt)
}

// admitLocked 是 L1 唯一的写入口（B2）：先让共享 L2 判定（快照走版本 CAS，删除走带版本删除），再按
// 判定写 L1——
//
//   - 接受：L1 记下这份，确认时刻为 L2 调用开始前（权威答案取更早的加载开始时刻）；
//   - stale（L2 已有更新的版本 / epoch / 墓碑）：改记 L2 当前的值（adoptSharedLocked）；
//   - 同版本异值：一致性错误，原样返回，L1 不写；
//   - 其他错误（断网、结果未知）：降级，L1 照记但未确认（权威答案除外）。
//
// L1 自己的准入（remoteSnapshotEntryStale / Conflict）仍在 AtomicLocal 分片锁下执行，所以 L1 已持有的
// 更新值不会被覆盖。调用方持有 key 的 publish 分片锁。
func (c *RemoteSnapshotCache) admitLocked(ctx context.Context, next remoteSnapshotEntry, authoritativeAt int64) error {
	started := c.nowNanos()
	err := c.writeShared(ctx, next)
	switch {
	case err == nil:
		next.confirmedAt = started
		if authoritativeAt != 0 && authoritativeAt < started {
			next.confirmedAt = authoritativeAt
		}
		return c.setL1Locked(ctx, next)
	case errors.Is(err, cache.ErrStaleWrite):
		return c.adoptSharedLocked(ctx, next, started)
	case errors.Is(err, ErrRemoteVersionConflict), errors.Is(err, cache.ErrConflictingWrite):
		return err
	default:
		c.remoteErrors.Add(1)
		next.confirmedAt = authoritativeAt
		return c.setL1Locked(ctx, next)
	}
}

// writeShared 把一个条目写到共享 L2（有界等待：调用方持有 publish 分片锁）。没有 L2 时 L1 自己就是水位。
func (c *RemoteSnapshotCache) writeShared(ctx context.Context, entry remoteSnapshotEntry) error {
	if c.l2 == nil {
		return nil
	}
	remoteCtx, cancel := context.WithTimeout(ctx, c.loadTimeout)
	defer cancel()
	if !entry.deleted {
		return c.l2.Set(remoteCtx, entry.snapshot)
	}
	if deleter, ok := c.l2.(RemoteSnapshotVersionedDeleter); ok {
		return deleter.DeleteAtVersion(remoteCtx, entry.snapshot.Key, entry.snapshot.StateVersion)
	}
	return c.l2.Delete(remoteCtx, entry.snapshot.Key)
}

// adoptSharedLocked 在 L2 以 stale 拒绝 refused 之后让 L1 跟上 L2：
//
//   - L2 持有值：记下它（确认时刻 started）。
//   - L2 已没有活值：墓碑不旧于被拒的写（或键恰好在两次调用之间过期）。L1 里不比被拒的写新的快照同样在
//     墓碑之后，删掉它——否则权威加载会把这份从未被确认的旧条目当作结果交出。不记删除标记：不知道墓碑的
//     确切版本，按被拒的版本记会挡住键过期后另一个 epoch 的合法写入。
//   - L2 读不到：L1 保持原样，被拒的那份是过去，L1 已有的条目照旧受陈旧上限约束。
//
// 调用方持有 key 的 publish 分片锁。
func (c *RemoteSnapshotCache) adoptSharedLocked(ctx context.Context, refused remoteSnapshotEntry, started int64) error {
	if c.l2 == nil {
		return nil
	}
	key := refused.snapshot.Key
	remoteCtx, cancel := context.WithTimeout(ctx, c.loadTimeout)
	stored, held, err := c.l2.Get(remoteCtx, key)
	cancel()
	if err != nil {
		c.remoteErrors.Add(1)
		return nil
	}
	if !held {
		if current, ok := c.l1Entry(ctx, key); ok && !current.deleted && !remoteSnapshotEntryStale(current, refused) {
			return c.l1.Delete(ctx, key)
		}
		return nil
	}
	if err := c.setL1Locked(ctx, remoteSnapshotEntry{snapshot: stored, confirmedAt: started}); err != nil && !errors.Is(err, ErrRemoteVersionConflict) {
		return err
	}
	return nil
}

// setL1Locked 写一个 L1 条目。L1 已持有更新的值（ErrStaleWrite）不是错误；同版本异值是一致性错误。
func (c *RemoteSnapshotCache) setL1Locked(ctx context.Context, entry remoteSnapshotEntry) error {
	ttl := c.ttl
	if entry.deleted {
		ttl = c.tombstoneTTL
	}
	err := c.l1.SetWithTTL(ctx, entry, ttl)
	switch {
	case err == nil, errors.Is(err, cache.ErrStaleWrite):
		return nil
	case errors.Is(err, cache.ErrConflictingWrite):
		return fmt.Errorf("%w: same snapshot version has different content", ErrRemoteVersionConflict)
	default:
		return err
	}
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
	current, ok, err := c.l1Snapshot(ctx, update.Key)
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
// Replication and commit paths must use DeleteAtVersion. An L2 failure is
// degraded around like every other L2 outage.
func (c *RemoteSnapshotCache) Delete(ctx context.Context, key RemoteSnapshotKey) error {
	if c == nil || c.l1 == nil || !key.Valid() {
		return nil
	}
	lock := &c.publishMu[remoteSnapshotPublishShard(key)]
	lock.Lock()
	defer lock.Unlock()
	if c.l2 != nil {
		remoteCtx, cancel := context.WithTimeout(ctx, c.loadTimeout)
		if err := c.l2.Delete(remoteCtx, key); err != nil {
			c.remoteErrors.Add(1)
		}
		cancel()
	}
	return c.l1.Delete(ctx, key)
}

// DeleteAtVersion applies a delete that happened at version (U-0187, RR-20260913-01). It goes through
// the same admission as every other write (B2): the shared L2 deletes unless it holds a newer snapshot
// and keeps a tombstone; L1 keeps a delete marker that fences snapshots not newer than version, unless
// L1 holds a newer snapshot. An L2 outage or unknown result leaves the marker unconfirmed, and the next
// read re-sends the versioned delete (idempotent) — A2 left "L2 DEL result unknown" to this layer.
func (c *RemoteSnapshotCache) DeleteAtVersion(ctx context.Context, key RemoteSnapshotKey, version uint64) error {
	if c == nil || c.l1 == nil || !key.Valid() {
		return nil
	}
	lock := &c.publishMu[remoteSnapshotPublishShard(key)]
	lock.Lock()
	defer lock.Unlock()
	marker := remoteSnapshotDeleteMarker(key, version)
	if current, ok := c.l1Entry(ctx, key); ok && current.confirmedAt != 0 {
		// L1 已确认持有更新的快照，或已确认同一个 / 更新的删除：这次删除是过去或重复。
		if remoteSnapshotEntryStale(current, marker) || (current.deleted && current.snapshot.StateVersion >= version) {
			return nil
		}
	}
	return c.admitLocked(ctx, marker, 0)
}

func (c *RemoteSnapshotCache) WaitForVersion(ctx context.Context, key RemoteSnapshotKey, minVersion uint64) error {
	if minVersion == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if snapshot, ok, _ := c.l1Snapshot(ctx, key); ok && snapshot.StateVersion >= minVersion {
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
	if snapshot, ok, _ := c.l1Snapshot(ctx, key); ok && snapshot.StateVersion >= minVersion {
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
	// 第二个返回值沿用 ReadThroughStats 的形状（B2 之前由 ReadThroughStore 统计）：权威加载、合并的
	// 等待、加载错误与 L2 错误次数。
	return c.l1.Stats(), cache.ReadThroughStats{
		Loads: c.authorityLoads.Load(), Coalesced: c.coalesced.Load(),
		LoadErrors: c.loadErrors.Load(), RemoteError: c.remoteErrors.Load(),
	}
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
