package remoteentity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"
	kitredis "github.com/tjbdwanghaibo/roost-core/kit/redis"
	coreremote "github.com/tjbdwanghaibo/roost-core/remoteentity"
)

// remote_entity.* 的声明（维护者决定 A4 ①）。数字与时长写 0 取 coreremote.DefaultConfig 的值（声明里 min:"0"），
// 负数拒绝；0 本身有含义的键（写许可、墓碑副本数、每节点配额）声明的 default 与 DefaultConfig 相同
// （TestRemoteEntityDeclaredDefaultsMatchCoreDefaults）。

// snapshotConfig 是快照段：缓存、共享 L2、陈旧上限、兴趣表、加载超时与等待者。RemoteEntityMod 与只读的
// RemoteMirrorMod 都匿名嵌入它，共用这一份声明。
type snapshotConfig struct {
	SnapshotCacheShards  int           `config:"snapshot_cache_shards" min:"0" example:"64"`
	SnapshotCacheEntries int           `config:"snapshot_cache_entries" min:"0" example:"10000"`
	SnapshotCacheBytes   int64         `config:"snapshot_cache_bytes" min:"0" example:"268435456"`
	SnapshotCacheTTL     time.Duration `config:"snapshot_cache_ttl" min:"0" example:"30s"`
	CachedMaxStaleness   time.Duration `config:"cached_max_staleness" min:"1ns" example:"30s" help:"B2：Cached / Monotonic 读交出的快照距最近一次被共享 L2 或权威确认的最长时间，超过先重新确认，\n确认不了就不交出。不配置时等于 snapshot_cache_ttl（这里写成与它相同）；配置了必须为正。"`
	SnapshotL2TTL        time.Duration `config:"snapshot_l2_ttl" min:"0" example:"10m"`
	SnapshotL2KeyPrefix  string        `config:"snapshot_l2_key_prefix" example:"" help:"Prefix for the shared L2 snapshot keys (remote_entity:snapshot:*). Empty keeps the\nunprefixed keys; deployments sharing one Redis db must set different values, e.g.\nroost:<project>. Every node of a deployment must use the same value; no hash tags."`
	// O-M6-3：L2 写墓碑之后等几个副本确认、最多等多久。超时必须为正（Redis 的 WAIT … 0 是永久阻塞）且不超过 1s。
	SnapshotL2TombstoneWaitReplicas int           `config:"snapshot_l2_tombstone_wait_replicas" default:"1" min:"0" example:"1" help:"O-M6-3：L2 写删除墓碑后用 WAIT 等几个副本确认、最多等多久，缩小切主时删除短暂复活的窗口。\n0 个副本即关闭；主节点没有副本时自动不等；确认不足只计数并记 Warn，不回滚。超时必须为正且不超过 1s。"`
	SnapshotL2TombstoneWaitTimeout  time.Duration `config:"snapshot_l2_tombstone_wait_timeout" default:"50ms" min:"1ns" max:"1s" example:"50ms"`
	SnapshotInterestTTL             time.Duration `config:"snapshot_interest_ttl" min:"0" example:"30s"`
	SnapshotInterestKeys            int           `config:"snapshot_interest_keys" min:"0" example:"10000"`
	SnapshotInterestSubs            int           `config:"snapshot_interest_subs" min:"0" example:"100000"`
	SnapshotInterestPerConsumer     int           `config:"snapshot_interest_per_consumer" min:"0" example:"0" help:"O4：每个 consumer 节点在兴趣表里的租约配额，0 取 snapshot_interest_subs / 16，不能超过 snapshot_interest_subs。\n超出配额的 key 没有推送、按需读取（计 interest_rejected_total{reason}）。"`
	MarkerCacheTTL                  time.Duration `config:"marker_cache_ttl" min:"0" example:"2s"`
	SnapshotLoadTimeout             time.Duration `config:"snapshot_load_timeout" min:"0" example:"3s"`
	SnapshotMaxWaiters              int           `config:"snapshot_max_waiters" min:"0" example:"4096"`
}

// mongoConfig 是 remote_entity.mongo.*：权威存储的库名与事务记录的保留期。
type mongoConfig struct {
	Database       string        `config:"database" default:"remote_entity" example:"remote_entity"`
	TransactionTTL time.Duration `config:"transaction_ttl" min:"0" example:"168h"`
}

// ValidateConfig：L2 键前缀的格式（RR-20260927-17）与每节点配额不超过兴趣表的每节点上限（O4）。
func (c *snapshotConfig) ValidateConfig(bool) error {
	var errs []error
	if c.SnapshotL2KeyPrefix != "" {
		if err := coreremote.ValidateSnapshotL2KeyPrefix(c.SnapshotL2KeyPrefix); err != nil {
			errs = append(errs, fmt.Errorf("remote_entity.snapshot_l2_key_prefix: %w", err))
		}
	}
	subs := c.SnapshotInterestSubs
	if subs <= 0 {
		subs = coreremote.DefaultConfig().SnapshotInterestSubs
	}
	if c.SnapshotInterestPerConsumer > subs {
		errs = append(errs, fmt.Errorf("remote_entity.snapshot_interest_per_consumer must be between 0 and remote_entity.snapshot_interest_subs (%d), got %d", subs, c.SnapshotInterestPerConsumer))
	}
	return errors.Join(errs...)
}

// apply 把快照段写进 out：0 保留 DefaultConfig 的值，声明了 default 的键原样写入。
func (c snapshotConfig) apply(out *coreremote.Config) {
	setPositive(&out.SnapshotCacheShards, c.SnapshotCacheShards)
	setPositive(&out.SnapshotCacheEntries, c.SnapshotCacheEntries)
	setPositive(&out.SnapshotCacheBytes, c.SnapshotCacheBytes)
	setPositive(&out.SnapshotCacheTTL, c.SnapshotCacheTTL)
	setPositive(&out.CachedMaxStaleness, c.CachedMaxStaleness)
	setPositive(&out.SnapshotL2TTL, c.SnapshotL2TTL)
	out.SnapshotL2KeyPrefix = c.SnapshotL2KeyPrefix
	out.SnapshotL2TombstoneWaitReplicas = c.SnapshotL2TombstoneWaitReplicas
	out.SnapshotL2TombstoneWaitTimeout = c.SnapshotL2TombstoneWaitTimeout
	setPositive(&out.SnapshotInterestTTL, c.SnapshotInterestTTL)
	setPositive(&out.SnapshotInterestKeys, c.SnapshotInterestKeys)
	setPositive(&out.SnapshotInterestSubs, c.SnapshotInterestSubs)
	out.SnapshotInterestPerConsumer = c.SnapshotInterestPerConsumer
	setPositive(&out.MarkerCacheTTL, c.MarkerCacheTTL)
	setPositive(&out.SnapshotLoadTimeout, c.SnapshotLoadTimeout)
	setPositive(&out.SnapshotMaxWaiters, c.SnapshotMaxWaiters)
}

func setPositive[T int | int64 | time.Duration](target *T, value T) {
	if value > 0 {
		*target = value
	}
}

// entityConfig 是 RemoteEntityMod 读的键。
type entityConfig struct {
	app.ServiceIdentity
	kitredis.ClusterConfig
	RemoteEntity struct {
		LockTTL                   time.Duration `config:"lock_ttl" min:"1ns" example:"15s"`
		LockKey                   string        `config:"lock_key" help:"锁身份键；Redis Cluster 下必须带非空 hash tag（例如 {roost:remote}），不写取 coreremote 的缺省"`
		RetryCount                int           `config:"retry_count" min:"0" example:"3"`
		RetryDelay                time.Duration `config:"retry_delay" min:"0" example:"100ms"`
		OpTimeout                 time.Duration `config:"op_timeout" min:"1ns" example:"3s"`
		UnlockRetryCount          int           `config:"unlock_retry_count" min:"0" example:"5"`
		UnlockRetryInterval       time.Duration `config:"unlock_retry_interval" min:"0" example:"100ms"`
		VersionTTL                time.Duration `config:"version_ttl" min:"0" example:"24h"`
		FinalizeRetryInterval     time.Duration `config:"finalize_retry_interval" min:"0" example:"500ms"`
		FinalizeProjectionTimeout time.Duration `config:"finalize_projection_timeout" min:"0"`
		MaxWriteBatch             int           `config:"max_write_batch" min:"0" example:"64"`
		MaxConcurrentWrites       int           `config:"max_concurrent_writes" default:"128" min:"0" help:"同时在途的 Remote 写许可；0 取 async_finalize_capacity，大于它时按它封顶；定容见 kit/README.md remoteentity“独立资源预算”"`
		AsyncFinalizeCapacity     int           `config:"async_finalize_capacity" min:"0" example:"4096"`
		AsyncFinalizeWorkers      int           `config:"async_finalize_workers" min:"0" example:"16"`
		OutboxPublishWorkers      int           `config:"outbox_publish_workers" default:"8" min:"0" max:"64" help:"唯一发布协调者内的独立事务并发；0取8，同Entity保序"`
		TransactionTrackLimit     int           `config:"transaction_track_limit" min:"0" example:"100000"`
		TransactionTrackTTL       time.Duration `config:"transaction_track_ttl" min:"0" example:"10m"`
		WrapperCapacity           int           `config:"wrapper_capacity" min:"0" example:"65536"`
		WrapperIdleTTL            time.Duration `config:"wrapper_idle_ttl" min:"0" example:"5m"`
		snapshotConfig
		Mongo mongoConfig `config:"mongo"`
	} `config:"remote_entity"`
}

// ValidateConfig：锁状态与 fence 在一个 Lua 里更新，Redis Cluster 下锁键必须显式带同槽的 hash tag；
// 不自动改键，否则滚动发布时新旧节点会锁住不同身份。
func (c *entityConfig) ValidateConfig(bool) error {
	if len(c.ClusterAddrs) == 0 {
		return nil
	}
	key := c.RemoteEntity.LockKey
	if key == "" {
		key = coreremote.DefaultConfig().LockKey
	}
	start := strings.IndexByte(key, '{')
	if start < 0 || strings.IndexByte(key[start+1:], '}') <= 0 {
		return fmt.Errorf("remote_entity: Redis Cluster requires a non-empty hash tag in remote_entity.lock_key (for example {roost:remote}); got %q", key)
	}
	return nil
}

// mirrorConfig 是 RemoteMirrorMod 读的键：快照段、库名与自己的停机预算；锁、提交、finalizer 的键不读。
type mirrorConfig struct {
	app.ServiceIdentity
	RemoteEntity struct {
		snapshotConfig
		Mongo  mongoConfig `config:"mongo"`
		Mirror struct {
			ShutdownTimeout time.Duration `config:"shutdown_timeout" default:"5s" min:"1ns" example:"5s" help:"Mirror 第 5 步：只读服务（kit/remoteentity.NewRemoteMirrorMod）声明给 App 的停机预算，RemoteEntityMod 不读它。\n生成的 shutdown.total_timeout 与部署宽限期只计入生成器认识的 Mod，手工接入 Mirror Mod 时要一并调大。"`
		} `config:"mirror"`
	} `config:"remote_entity"`
}
