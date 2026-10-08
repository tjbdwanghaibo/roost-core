package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/cache"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// ErrSnapshotClientStopped 表示 SnapshotClient 已停止：不再读、不再访问 L2 / 权威 / 总线。停止是单次的，
// 重新运行要新建客户端。
var ErrSnapshotClientStopped = errors.New("remote_entity: snapshot client is stopped")

// SnapshotClient 是 Remote 快照协议的唯一实现（Mirror 方案第 3 步，docs/feature/MIRROR-STEPS-1-3-2026-10-06.md）：
//
//   - 读：ReadSnapshot / ReadRemoteSnapshot，经快照缓存唯一的读出口 RemoteSnapshotCache.Read；
//   - 兴趣：读时续租本机兴趣并广播，全集群兴趣表决定 owner 发布哪些 key；
//   - 按 key apply：复制消息（SnapshotReplicaStore）与兴趣消息（InterestReplicaStore）都落到这里；
//   - 回填：缺基 / epoch / schema 不符时经权威 loader 回填；
//   - 状态与生命周期：Stats；Start 订阅两个复制主题，Stop 三步停机。
//
// 它不要求写 backend：依赖只有可选的共享 L2、可选的权威 loader 和同步总线。Manager（写 owner）组合一个
// SnapshotClient 并委托它，owner 的提交后发布走包内入口 publishCommitted，只读方拿不到。
//
// 本类型不直接写 L1 / L2：全部缓存写入经 RemoteSnapshotCache 的公开入口（Publish / ApplyReplica /
// DeleteAtVersion / Read / LoadAuthoritative），新值由其中的 admitLocked 先经 L2 准入（B2）；缓存内部
// 直接写 L1 的几处只回填 L2 的值或删除，清单见 RemoteSnapshotCache 类型注释。
type SnapshotClient struct {
	cfg          *Config
	consumerSID  int32
	linearizable bool
	cache        *entity.RemoteSnapshotCache
	interests    *remoteInterestRegistry

	// interestGeneration stamps this consumer's renewals and releases. Seeded
	// from the clock so a restart that reuses the SID starts above anything
	// the previous process could have issued, instead of at zero where an
	// old release in flight would outrank every new renewal (RR-20260913-02).
	interestGeneration atomic.Uint64
	// 本机兴趣：key → 本机认为的租约到期时刻。续租在剩余时间不足一半时才广播（避免每次读一条消息）。
	localInterestMu       sync.Mutex
	localInterestLocks    [64]sync.Mutex
	localInterests        map[entity.RemoteSnapshotKey]int64
	localInterestOps      atomic.Uint64
	localInterestCapacity int

	// transport 在装配期设置（BindSync / Start / SetSyncer），之后只读。
	transport remoteSyncTransport

	// push 报告快照推送是否开着（Mirror 第 4 步）：Start 在能确认订阅的总线（fsyncbus.ILiveSubscriber，
	// JetStream）上订阅快照主题并置 true；普通 NATS 上不订阅、记 Warn，读取按陈旧上限回源（按需读取）。
	push atomic.Bool
	// interestRejected 计本机兴趣续租被拒（配额、表满）的次数（O4）；被拒的 key 没有推送，按需读取。
	interestRejected atomic.Uint64

	// 生命周期。mu 只保护下面的字段，不在持有它时等待。
	mu          sync.Mutex
	bus         fsyncbus.ISyncBus
	snapshotRep *mirror.Replicator
	interestRep *mirror.Replicator
	// refreshRep 订阅兴趣续租请求（O-M6-1，interest_refresh.go）；只在快照推送开着时建立。
	refreshRep *mirror.Replicator
	started    bool
	stopped    atomic.Bool
	// work 是访问依赖（L2、权威 loader、兴趣广播）的准入与在途计数（共用 operation.Lifetime，A3）。
	// Stop 关闭它并在调用方 ctx 内等在途调用返回；返回 nil 之后调用方才能释放 Redis / 权威 / 总线。
	work operation.Lifetime
	// stopCtx 在 Stop 时取消，在途的权威加载据此放弃（loader 不响应取消时 Stop 如实超时）。
	stopCtx    context.Context
	stopCancel context.CancelFunc

	// 兴趣续租请求的遍历状态（O-M6-1）：同一时刻最多一个遍历，进行中到达的请求只置 refreshPending。
	refreshMu        sync.Mutex
	refreshRunning   bool
	refreshPending   bool
	refreshLastStart time.Time

	// 读路径只登记兴趣；有界队列由至多一个 worker 广播，空闲即退出。
	interestPublishMu      sync.Mutex
	interestPublishQueue   chan entity.RemoteSnapshotInterest
	interestPublishRunning bool
}

// SnapshotClientDeps 是只读客户端的依赖。没有写 backend、锁或 finalizer。
type SnapshotClientDeps struct {
	// L2 是共享快照层（生产为 Redis，NewSnapshotL2StoreWithKeyPrefix）。nil 时 L1 自己是水位，只适合
	// 单进程或测试。
	L2 cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]
	// Loader 是权威加载（缺基回填、Monotonic 不足、L2 担保不了时回源）。nil 时只读缓存与 L2。
	Loader entity.RemoteSnapshotLoader
	// LinearizableLoader 声明 Loader 提供线性化读。false 时 Linearizable 读返回
	// entity.ErrRemoteReadUnsupported，不静默退化。
	LinearizableLoader bool
	// ConsumerSID 是兴趣的消费者身份（本服 sid），必须非零。
	ConsumerSID int32
}

// NewSnapshotClient 构造只读客户端。cfg 为 nil 时用 DefaultConfig；非法组合直接拒绝。
func NewSnapshotClient(cfg *Config, deps SnapshotClientDeps) (*SnapshotClient, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if err := validateSnapshotClientConfig(cfg, deps); err != nil {
		return nil, err
	}
	return newSnapshotClient(cfg, deps), nil
}

func validateSnapshotClientConfig(cfg *Config, deps SnapshotClientDeps) error {
	switch {
	case deps.ConsumerSID == 0:
		return errors.New("remote_entity: snapshot client needs a non-zero consumer sid")
	case deps.LinearizableLoader && deps.Loader == nil:
		return errors.New("remote_entity: a linearizable snapshot client needs a loader")
	}
	return validateSnapshotConfig(cfg, deps.L2 != nil)
}

// validateSnapshotConfig 校验快照段配置。只读方（NewSnapshotClient）与写 owner（Assemble）共用同一套规则
// （RR-20261006-71）：之前只有只读路径校验，owner 装配静默接受 SnapshotInterestTTL = 0（兴趣一律拒绝、
// owner 永不推送）、SnapshotL2TTL = 0（墓碑不落地、O5 过滤关闭）这类组合。
func validateSnapshotConfig(cfg *Config, hasL2 bool) error {
	switch {
	case cfg.SnapshotInterestTTL <= 0:
		return errors.New("remote_entity: snapshot_interest_ttl must be positive")
	case cfg.SnapshotLoadTimeout <= 0:
		return errors.New("remote_entity: snapshot_load_timeout must be positive")
	case cfg.SnapshotCacheTTL < 0 || cfg.CachedMaxStaleness < 0:
		return errors.New("remote_entity: snapshot cache ttl and cached_max_staleness must not be negative")
	case hasL2 && cfg.SnapshotL2TTL <= 0:
		return errors.New("remote_entity: snapshot_l2_ttl must be positive when a shared L2 is configured")
	case cfg.SnapshotInterestPerConsumer < 0 || (cfg.SnapshotInterestSubs > 0 && cfg.SnapshotInterestPerConsumer > cfg.SnapshotInterestSubs):
		return fmt.Errorf("remote_entity: snapshot_interest_per_consumer (%d) must be between 0 and snapshot_interest_subs (%d)", cfg.SnapshotInterestPerConsumer, cfg.SnapshotInterestSubs)
	}
	return nil
}

// newSnapshotClient 是不校验的构造（Manager 内嵌用，保持 NewManager 不返回错误的旧签名）；正式的 owner 装配
// 入口 Assemble 先经 validateSnapshotConfig 校验同一组规则。
func newSnapshotClient(cfg *Config, deps SnapshotClientDeps) *SnapshotClient {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	c := &SnapshotClient{
		cfg: cfg, consumerSID: deps.ConsumerSID, linearizable: deps.LinearizableLoader,
		interests: newRemoteInterestRegistry(remoteInterestLimits{
			PerConsumer: cfg.SnapshotInterestPerConsumer, Total: cfg.SnapshotInterestSubs, ReleaseFence: cfg.SnapshotInterestTTL,
		}),
		localInterests:        make(map[entity.RemoteSnapshotKey]int64),
		localInterestCapacity: cfg.SnapshotInterestKeys,
		stopCtx:               stopCtx, stopCancel: stopCancel,
	}
	var l2 cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]
	if deps.L2 != nil {
		l2 = gatedSnapshotL2{client: c, next: deps.L2}
	}
	var loader entity.RemoteSnapshotLoader
	if deps.Loader != nil {
		loader = c.gatedLoader(deps.Loader)
	}
	c.cache = entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{
		Shards: cfg.SnapshotCacheShards, MaxEntries: cfg.SnapshotCacheEntries,
		MaxBytes: cfg.SnapshotCacheBytes, TTL: cfg.SnapshotCacheTTL,
		LoadTimeout: cfg.SnapshotLoadTimeout, MaxWaiters: cfg.SnapshotMaxWaiters,
		MaxStaleness: cfg.CachedMaxStaleness, ReplicaBuffer: cfg.SnapshotReplicaBuffer,
	}, l2, loader)
	return c
}

var _ entity.RemoteSnapshotReadOnly = (*SnapshotClient)(nil)

// ReadSnapshot 是只读读取（entity.RemoteSnapshotReadOnly）。读时续租这个完整 key 的兴趣（失败只意味着
// 之后回源，不影响这次读）；快照的全部后置条件由缓存唯一的读出口保证。
func (c *SnapshotClient) ReadSnapshot(ctx context.Context, req entity.RemoteSnapshotRead) (snapshot entity.RemoteSnapshotEnvelope, found bool, err error) {
	started := time.Now()
	defer func() {
		result := "hit"
		if err != nil {
			result = "error"
		} else if !found {
			result = "miss"
		}
		labels := metrics.Labels{"result": result, "consistency": remoteConsistencyLabel(req.Consistency)}
		metrics.IncCounter("remote_entity.remote.read_total", labels, 1)
		metrics.ObserveDuration("remote_entity.remote.read_latency", labels, time.Since(started))
	}()
	if c == nil || c.cache == nil || !req.Key.Valid() {
		return entity.RemoteSnapshotEnvelope{}, false, entity.ErrRemoteRejected
	}
	if c.stopped.Load() {
		return entity.RemoteSnapshotEnvelope{}, false, ErrSnapshotClientStopped
	}
	if req.Consistency == entity.RemoteReadLinearizable && !c.linearizable {
		return entity.RemoteSnapshotEnvelope{}, false, entity.ErrRemoteReadUnsupported
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 续租失败（O4 配额、表满、总线不可用）不影响这次读：这个 key 没有推送刷新确认时刻，读取在陈旧上限
	// 之后经 L2 / 权威重新确认——按需读取。被拒在 RenewInterest 里计数。
	if err := ctx.Err(); err != nil {
		return entity.RemoteSnapshotEnvelope{}, false, err
	}
	c.queueReadInterest(req.Key)
	return c.cache.Read(ctx, req)
}

// ReadRemoteSnapshot 是旧签名（entity.RemoteSnapshotReader）：minVersion 是只约束版本的 token，
// consistency 必须显式给出。
func (c *SnapshotClient) ReadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	if consistency == 0 {
		return entity.RemoteSnapshotEnvelope{}, false, entity.ErrRemoteReadConsistency
	}
	return c.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: consistency, After: entity.RemoteObservation{StateVersion: minVersion}})
}

func remoteConsistencyLabel(consistency entity.RemoteReadConsistency) string {
	switch consistency {
	case entity.RemoteReadCached, 0:
		return "cached"
	case entity.RemoteReadLinearizable:
		return "linearizable"
	default:
		return "monotonic"
	}
}

// SnapshotClientStats 是健康检查用的容量与模式。
type SnapshotClientStats struct {
	LocalInterests int
	// PushEnabled 报告快照推送是否开着（Start 在能确认订阅的总线上）；false 时全部读取按需回源。
	PushEnabled bool
	// InterestRejected 是本机兴趣续租被拒的累计次数（O4：配额或表满；这些 key 按需读取）。
	InterestRejected uint64
	// Bootstrap 是首载缓冲的累计计数（Mirror 第 4 步）。
	Bootstrap entity.RemoteSnapshotBootstrapStats
}

// Stats 返回本机兴趣数。RR-20261005-NC-131：过期条目只在新建兴趣且表满、或每 1024 次续租时清理，空闲进程
// 里会一直留着；健康检查拿这个数和上限比，所以表满时先清掉过期的再数。只在表满时清理，平时不全表扫描。
func (c *SnapshotClient) Stats() SnapshotClientStats {
	if c == nil {
		return SnapshotClientStats{}
	}
	c.localInterestMu.Lock()
	defer c.localInterestMu.Unlock()
	if capacity := c.localInterestCapacity; capacity > 0 && len(c.localInterests) >= capacity {
		c.pruneLocalInterestsLocked(time.Now().UnixNano())
	}
	return SnapshotClientStats{
		LocalInterests: len(c.localInterests), PushEnabled: c.push.Load(),
		InterestRejected: c.interestRejected.Load(), Bootstrap: c.cache.BootstrapStats(),
	}
}

// ---- 兴趣 ----

// RenewInterest 续租本机对 key 的兴趣：剩余不足一半才登记并广播新的 generation。
func (c *SnapshotClient) RenewInterest(ctx context.Context, key entity.RemoteSnapshotKey) error {
	_, err := c.renewInterest(ctx, key, false)
	return err
}

// renewInterest 是续租的唯一入口。refresh 为 true 时是兴趣续租请求触发的重新续租（O-M6-1）：不看“剩余
// 不足一半”的门槛，但只续本机表里仍然有效的 key——遍历开始之后被 release 或已过期的 key 不归它复活。
// 其余（条带锁内分配代际、本机兴趣表按同一配额判定、锁外广播、失败回滚）与读时续租完全相同。
// 返回是否广播了一条续租。
func (c *SnapshotClient) renewInterest(ctx context.Context, key entity.RemoteSnapshotKey, refresh bool) (bool, error) {
	if c == nil || !key.Valid() {
		return false, entity.ErrRemoteRejected
	}
	interest, ok, err := c.admitInterestRenewal(key, refresh)
	if !ok {
		return false, err
	}
	// 广播在条带锁外（RR-20261006-68）：之前在锁内同步发布，总线慢时（最长 syncbus.publish_timeout）同条带
	// 其他 key 的读——包括不需要续租的 L1 命中——都排队等这一次发布。锁外发布不破坏顺序：线上的 renew /
	// release 带锁内分配的代际，接收端按代际判新旧（renewIfNeeded 的撤销水位、release 的代际比较），到达
	// 顺序与分配顺序不同也收敛到同一结果。同一个 key 的其他读在发布期间看到本机表里的新租约，不再重复续租。
	if err := c.publishInterest(ctx, interest, false); err != nil {
		// 回滚只撤掉这一次续租（按 ExpiresAt 比对）：发布期间同 key 已被 release 或更新的续租覆盖时不动。
		c.rollbackLocalInterest(key, interest.ExpiresAt)
		return false, err
	}
	if c.localInterestOps.Add(1)&1023 == 0 {
		c.localInterestMu.Lock()
		c.pruneLocalInterestsLocked(time.Now().UnixNano())
		c.localInterestMu.Unlock()
	}
	return true, nil
}

// admitInterestRenewal 在条带锁内分配代际并更新本机表与本机兴趣表；ok 为 false 时不需要（或不能）广播，
// err 是被拒的原因。广播由调用方在锁外做。
func (c *SnapshotClient) admitInterestRenewal(key entity.RemoteSnapshotKey, refresh bool) (entity.RemoteSnapshotInterest, bool, error) {
	ttl := c.cfg.SnapshotInterestTTL
	stripe := &c.localInterestLocks[uint64(key.EntityID)%uint64(len(c.localInterestLocks))]
	stripe.Lock()
	defer stripe.Unlock()
	// generation 在条带锁内分配（Mirror 第 4 步）：同一 key 的 renew / release 按锁的顺序拿到递增的代际，
	// 本机表与兴趣表按同一顺序收敛。之前在锁外分配，并发的 renew(g) 与 release(g+1) 可能按相反顺序执行：
	// 本机表记着租约，兴趣表与 owner 却按 g+1 撤销了。
	now := time.Now().UnixNano()
	interest := entity.RemoteSnapshotInterest{ConsumerSID: c.consumerSID, Key: key, ExpiresAt: now + ttl.Nanoseconds(), Generation: c.nextInterestGeneration()}
	c.localInterestMu.Lock()
	current, loaded := c.localInterests[key]
	switch {
	case refresh && (!loaded || current <= now):
		c.localInterestMu.Unlock()
		return interest, false, nil
	case !refresh && loaded && current-now > (ttl/2).Nanoseconds():
		c.localInterestMu.Unlock()
		return interest, false, nil
	}
	if !loaded && c.localInterestCapacity > 0 && len(c.localInterests) >= c.localInterestCapacity {
		c.pruneLocalInterestsLocked(now)
		if len(c.localInterests) >= c.localInterestCapacity {
			c.localInterestMu.Unlock()
			c.noteInterestRejected("local_table_full")
			return interest, false, entity.ErrRemoteOverloaded
		}
	}
	c.localInterests[key] = interest.ExpiresAt
	c.localInterestMu.Unlock()
	// 本机的兴趣表收到自己的全部续租，与 owner 按同一配额判定（O4）：在这里被拒，owner 也会拒，不再广播。
	if err := c.interests.renew(interest); err != nil {
		c.rollbackLocalInterest(key, interest.ExpiresAt)
		if errors.Is(err, entity.ErrRemoteOverloaded) {
			c.noteInterestRejected("registry")
		}
		return interest, false, err
	}
	return interest, true, nil
}

// ReleaseInterest 撤销本机对 key 的兴趣（带新的 generation，只撤销不新于它的租约）。
func (c *SnapshotClient) ReleaseInterest(ctx context.Context, key entity.RemoteSnapshotKey) error {
	if c == nil || !key.Valid() {
		return entity.ErrRemoteRejected
	}
	stripe := &c.localInterestLocks[uint64(key.EntityID)%uint64(len(c.localInterestLocks))]
	stripe.Lock()
	interest := entity.RemoteSnapshotInterest{ConsumerSID: c.consumerSID, Key: key, ExpiresAt: time.Now().UnixNano(), Generation: c.nextInterestGeneration()}
	c.localInterestMu.Lock()
	delete(c.localInterests, key)
	c.localInterestMu.Unlock()
	c.interests.release(key, c.consumerSID, interest.Generation)
	stripe.Unlock()
	// 与续租相同，广播在条带锁外（RR-20261006-68）；接收端按代际判新旧。
	return c.publishInterest(ctx, interest, true)
}

// noteInterestRejected 计一次本机续租被拒（O4）。日志由兴趣表限频记录；这里只计数。
func (c *SnapshotClient) noteInterestRejected(reason string) {
	c.interestRejected.Add(1)
	metrics.IncCounter("remote_entity.remote.interest_renew_refused_total", metrics.Labels{"reason": reason}, 1)
}

// publishInterest 经同步总线广播兴趣；它访问总线，所以受 work 准入约束。
func (c *SnapshotClient) publishInterest(ctx context.Context, interest entity.RemoteSnapshotInterest, release bool) error {
	publisher, ok := c.transport.(remoteInterestPublisher)
	if !ok {
		return nil
	}
	if !c.work.Begin() {
		return ErrSnapshotClientStopped
	}
	defer c.work.End()
	return publisher.PublishRemoteInterest(ctx, interest, release)
}

// nextInterestGeneration issues the next generation for this consumer's
// interest messages. The first call seeds from the clock (see
// interestGenerationClock).
func (c *SnapshotClient) nextInterestGeneration() uint64 {
	for {
		current := c.interestGeneration.Load()
		if current == 0 {
			seed := seedInterestGeneration(interestGenerationNow())
			if !c.interestGeneration.CompareAndSwap(0, seed) {
				continue
			}
		}
		generation := c.interestGeneration.Add(1)
		noteInterestGenerationIssued(generation)
		return generation
	}
}

func (c *SnapshotClient) rollbackLocalInterest(key entity.RemoteSnapshotKey, expiresAt int64) {
	c.localInterestMu.Lock()
	if c.localInterests[key] == expiresAt {
		delete(c.localInterests, key)
		// This process withdrawing its own lease locally: no message was
		// reordered, so the latest generation it issued is the right stamp,
		// and no release watermark is needed.
		c.interests.drop(key, c.consumerSID, c.interestGeneration.Load())
	}
	c.localInterestMu.Unlock()
}

func (c *SnapshotClient) pruneLocalInterestsLocked(now int64) {
	for key, expiresAt := range c.localInterests {
		if expiresAt <= now {
			delete(c.localInterests, key)
			c.interests.drop(key, c.consumerSID, c.interestGeneration.Load())
		}
	}
}

// ---- owner 侧（包内入口） ----

// publishCommitted 是 owner 提交后的发布：先经缓存新值的唯一写入口记下（admitLocked，L2 CAS 在前），再按兴趣
// 广播；删除同理，带提交版本。只有 Manager（写 owner）调用；只读方的公开 API 里没有它。
func (c *SnapshotClient) publishCommitted(ctx context.Context, commit entity.RemoteCommit) error {
	publisher, publish := c.transport.(entity.IRemoteSnapshotPublisher)
	for _, record := range commit.Snapshots {
		envelope := entity.RemoteSnapshotEnvelope{
			Key: record.Key, BaseVersion: record.BaseVersion, StateVersion: record.StateVersion,
			MarkerEpoch: record.MarkerEpoch, RouteEpoch: record.RouteEpoch,
			Schema: record.Schema, Codec: record.Codec, Checksum: record.Checksum, Full: record.Full,
			PublishedAt: time.Now().UnixNano(), Payload: entity.CopyFrozenRemoteSnapshotPayload(record.Data),
		}
		if err := c.cache.Publish(ctx, envelope); err != nil {
			return err
		}
		if publish {
			if err := publisher.PublishRemoteSnapshot(ctx, record.Clone()); err != nil {
				return err
			}
		}
	}
	for _, key := range commit.Invalidations {
		// Same rule as the replica side (U-0187): the local copy is deleted
		// at the commit's version so a concurrent older publish for the key
		// cannot repopulate it behind this commit.
		if err := c.cache.DeleteAtVersion(ctx, key, commit.NextVersion); err != nil {
			return err
		}
		if publish {
			if err := publisher.DeleteRemoteSnapshot(ctx, key, commit.NextVersion); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- 生命周期 ----

// bindLocked 在 bus 上建两个复制器并装上发布用的 transport；复制器未启动。live 为 true 时快照复制器用
// 可确认订阅（mirror.NewLive）。调用方持有 mu。
func (c *SnapshotClient) bindLocked(bus fsyncbus.ISyncBus, live bool) (snapshotRep, interestRep *mirror.Replicator) {
	if live {
		snapshotRep = mirror.NewLive(bus, SyncTopicSnapshot, SnapshotReplicaStore{client: c})
	} else {
		snapshotRep = mirror.New(bus, SyncTopicSnapshot, SnapshotReplicaStore{client: c})
	}
	interestRep = mirror.New(bus, SyncTopicInterest, InterestReplicaStore{client: c})
	c.bus, c.snapshotRep, c.interestRep = bus, snapshotRep, interestRep
	c.refreshRep = nil
	if live {
		// 兴趣续租请求（O-M6-1）只对当下有意义：用可确认订阅（JetStream DeliverNew），新 sid 不重放历史请求。
		c.refreshRep = mirror.NewLive(bus, SyncTopicInterestRefresh, InterestRefreshStore{client: c})
	}
	c.transport = &remoteSyncer{snapshotRep: snapshotRep, interestRep: interestRep, client: c}
	return snapshotRep, interestRep
}

// Start 订阅快照、兴趣与兴趣续租请求（O-M6-1，只在推送开着时）三个复制主题（幂等）。任一订阅失败时
// 退掉已建立的再返回：失败不留下订阅，重试不会重复订阅。失败重试复用首次绑定的 bus；换 bus 要新建客户端。停止之后不能再启动。
//
// 快照推送只在总线能确认订阅（fsyncbus.ILiveSubscriber，JetStream 的 DeliverNew）时开启（Mirror 第 4 步）：
// 订阅确认之后发布的快照不会被静默丢掉，加载在途时到达的进首载缓冲。普通 NATS 是最多一次，推送随时可能
// 静默丢失，所以不订阅快照主题，记一条 Warn，Stats().PushEnabled 为 false：读取按 cached_max_staleness
// 经 L2 / 权威回源（按需读取），正确性不依赖推送。兴趣主题照旧订阅（owner 按它决定发布，供 BindSync 的
// 旧接收方使用）。
func (c *SnapshotClient) Start(bus fsyncbus.ISyncBus) error {
	if c == nil {
		return errors.New("remote_entity: snapshot client is nil")
	}
	if bus == nil {
		return errors.New("remote_entity: sync bus is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped.Load() {
		return ErrSnapshotClientStopped
	}
	if c.started {
		return nil
	}
	if c.snapshotRep == nil {
		_, live := bus.(fsyncbus.ILiveSubscriber)
		c.bindLocked(bus, live)
	}
	push := c.snapshotRep.Live()
	if push {
		if err := c.snapshotRep.Start(); err != nil {
			return fmt.Errorf("remote_entity: start snapshot replica: %w", err)
		}
	}
	if err := c.interestRep.Start(); err != nil {
		c.snapshotRep.Stop()
		return fmt.Errorf("remote_entity: start interest replica: %w", err)
	}
	// 兴趣续租请求只在推送开着时有用（O-M6-1）：推送关着时 owner 不推，续租也就不必赶。
	if push && c.refreshRep != nil {
		if err := c.refreshRep.Start(); err != nil {
			c.snapshotRep.Stop()
			c.interestRep.Stop()
			return fmt.Errorf("remote_entity: start interest refresh replica: %w", err)
		}
	}
	c.started = true
	c.push.Store(push)
	pushGauge := int64(0)
	if push {
		pushGauge = 1
	}
	metrics.SetGauge("remote_entity.snapshot_push_enabled", metrics.Labels{"sid": fmt.Sprint(c.consumerSID)}, pushGauge)
	if !push {
		slog.Warn("remote_entity: snapshot push disabled: the sync bus cannot confirm subscriptions (JetStream required); Cached and Monotonic reads re-confirm through the shared L2 / authority after cached_max_staleness",
			"consumer_sid", c.consumerSID, "cached_max_staleness", c.cfg.CachedMaxStaleness, "snapshot_cache_ttl", c.cfg.SnapshotCacheTTL)
	}
	return nil
}

// unsubscribe 退掉两个订阅、不等待在途 handler，客户端仍可用、可再次 Start（Assembly 启动后续步骤失败时
// 的回收）。留下的在途 handler 由之后的 Stop 一并等待。
func (c *SnapshotClient) unsubscribe() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rep := range []*mirror.Replicator{c.snapshotRep, c.interestRep, c.refreshRep} {
		if rep != nil {
			rep.Stop()
		}
	}
	c.started = false
	c.push.Store(false)
}

// Stop 按三步停机（roost-coding；契约骨架 internal/stopcontract）：
//
//  1. 发起关闭（幂等）：之后的读返回 ErrSnapshotClientStopped；关闭依赖准入；取消在途的权威加载与
//     兴趣续租遍历（O-M6-1）；退掉复制订阅。
//  2. 在 ctx 内等待排空：已准入的复制 handler、兴趣续租遍历与依赖调用（L2、权威、兴趣广播）全部返回。ctx 先结束返回
//     ctx 错误，客户端保持“停止中”，用新 ctx 再调用会继续等同一批。
//  3. 返回 nil 之后调用方才能释放 Redis / 权威 / 总线。不响应取消的 loader 不会被杀，Stop 如实超时。
func (c *SnapshotClient) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	c.stopped.Store(true)
	c.started = false
	c.push.Store(false)
	reps := []*mirror.Replicator{c.snapshotRep, c.interestRep, c.refreshRep}
	c.mu.Unlock()
	c.work.Stop()
	c.stopCancel()
	for _, rep := range reps {
		if rep != nil {
			rep.Stop()
		}
	}
	for _, rep := range reps {
		if rep == nil {
			continue
		}
		if err := rep.StopWithContext(ctx); err != nil {
			return err
		}
	}
	return c.work.Wait(ctx)
}

// ---- 依赖准入 ----

// gatedLoader 让权威加载受 work 准入约束，并在 Stop 时取消。
func (c *SnapshotClient) gatedLoader(loader entity.RemoteSnapshotLoader) entity.RemoteSnapshotLoader {
	return func(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		if !c.work.Begin() {
			return entity.RemoteSnapshotEnvelope{}, false, ErrSnapshotClientStopped
		}
		defer c.work.End()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stopWatch := context.AfterFunc(c.stopCtx, cancel)
		defer stopWatch()
		return loader(ctx, key, consistency, minVersion)
	}
}

// gatedSnapshotL2 让共享 L2 的每次调用受 work 准入约束（L2 调用本身已由缓存的 LoadTimeout 限时，不另外
// 绑定取消）。停止之后 L2 调用返回 ErrSnapshotClientStopped，缓存按 L2 不可用降级（B2）：写入记为未确认，
// 读取不交出未确认的条目。
type gatedSnapshotL2 struct {
	client *SnapshotClient
	next   cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]
}

func (g gatedSnapshotL2) Get(ctx context.Context, key entity.RemoteSnapshotKey) (entity.RemoteSnapshotEnvelope, bool, error) {
	if !g.client.work.Begin() {
		return entity.RemoteSnapshotEnvelope{}, false, ErrSnapshotClientStopped
	}
	defer g.client.work.End()
	return g.next.Get(ctx, key)
}

func (g gatedSnapshotL2) Set(ctx context.Context, value entity.RemoteSnapshotEnvelope) error {
	if !g.client.work.Begin() {
		return ErrSnapshotClientStopped
	}
	defer g.client.work.End()
	return g.next.Set(ctx, value)
}

func (g gatedSnapshotL2) Delete(ctx context.Context, key entity.RemoteSnapshotKey) error {
	if !g.client.work.Begin() {
		return ErrSnapshotClientStopped
	}
	defer g.client.work.End()
	return g.next.Delete(ctx, key)
}

// DeleteAtVersion 转发带版本删除；下层没有这项能力时与缓存原来的退化相同（无条件删除）。
func (g gatedSnapshotL2) DeleteAtVersion(ctx context.Context, key entity.RemoteSnapshotKey, version uint64) error {
	if !g.client.work.Begin() {
		return ErrSnapshotClientStopped
	}
	defer g.client.work.End()
	if deleter, ok := g.next.(entity.RemoteSnapshotVersionedDeleter); ok {
		return deleter.DeleteAtVersion(ctx, key, version)
	}
	return g.next.Delete(ctx, key)
}
