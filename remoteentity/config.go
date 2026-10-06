package remoteentity

import "time"

// Config holds configuration for the remote entity module.
type Config struct {
	MaxWriteBatch        int
	SnapshotCacheShards  int
	SnapshotCacheEntries int
	SnapshotCacheBytes   int64
	SnapshotCacheTTL     time.Duration
	SnapshotL2TTL        time.Duration
	// SnapshotL2KeyPrefix 是共享 L2 快照键的可选部署前缀（kit：remote_entity.snapshot_l2_key_prefix）。
	// 空（默认）时键为 remote_entity:snapshot:…，与旧版本逐字相同、无需迁移；共用一个 Redis db 的多个部署
	// 应各自配置不同前缀，否则彼此读写同一份 L2 快照（RR-20260927-17）。同一部署的所有节点必须一致。
	SnapshotL2KeyPrefix string
	// SnapshotL2TombstoneWaitReplicas 是 L2 带版本删除（写墓碑）之后用 WAIT 等几个副本确认（O-M6-3，kit：
	// remote_entity.snapshot_l2_tombstone_wait_replicas）。0 关闭；DefaultConfig 为 1。普通快照写入不等。
	// 主节点没有连着的副本时自动不等；确认不足不回滚、不报错，只计数与记日志
	// （docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md §3）。
	SnapshotL2TombstoneWaitReplicas int
	// SnapshotL2TombstoneWaitTimeout 是上面 WAIT 的超时（kit：remote_entity.snapshot_l2_tombstone_wait_timeout），
	// 调用方最多多等这么久。等待副本时必须为正（Redis 的 WAIT … 0 是永久阻塞）；DefaultConfig 为 50ms。
	SnapshotL2TombstoneWaitTimeout time.Duration
	SnapshotInterestTTL            time.Duration
	// SnapshotInterestKeys 是本机（consumer 侧）兴趣表的 key 上限。
	SnapshotInterestKeys int
	// SnapshotInterestSubs 是每个节点全集群兴趣表的条目上限（内存上限；兴趣是广播，每个节点存所有
	// consumer 的租约）。按 consumer 数 × SnapshotInterestPerConsumer 选取。
	SnapshotInterestSubs int
	// SnapshotInterestPerConsumer 是每个 consumer 节点在兴趣表里的租约配额（O4，kit：
	// remote_entity.snapshot_interest_per_consumer）。零值取 SnapshotInterestSubs / 16；不能大于
	// SnapshotInterestSubs。超出配额的 key 没有推送，按需读取（docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md）。
	SnapshotInterestPerConsumer int
	// SnapshotReplicaBuffer 是一个 key 的权威加载在途时缓冲复制消息的条数（首载缓冲，Mirror 第 4 步）。
	// 零值取 64；溢出时丢弃缓冲、加载装入后再回源一次。
	SnapshotReplicaBuffer int
	MarkerCacheTTL        time.Duration
	SnapshotLoadTimeout   time.Duration
	// CachedMaxStaleness 是 Cached / Monotonic 读能交出的快照距最近一次被共享 L2 或权威确认的最长时间
	// （kit：remote_entity.cached_max_staleness，B2）。超过它的 L1 条目先重新确认（读 L2，必要时回源权威），
	// 确认不了就不交出。零值取 SnapshotCacheTTL。不覆盖 L2 本身落后于权威的情形（owner 写 L2 失败或
	// 结果未知时最长到 SnapshotL2TTL），见 docs/feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md。
	CachedMaxStaleness time.Duration
	SnapshotMaxWaiters int
	// MaxConcurrentWrites 限制从 Prepare 到真正释放的写批次，包括后台收尾。
	// 不等待额度；满额直接 ErrRemoteOverloaded。零值沿用收尾容量，默认配置为 128。
	MaxConcurrentWrites   int
	AsyncFinalizeCapacity int
	AsyncFinalizeWorkers  int
	TransactionTrackLimit int
	TransactionTrackTTL   time.Duration
	FinalizeRetryInterval time.Duration
	// FinalizeProjectionTimeout 是 Durability 1/2/3（async、strict、带 Remote 批次的 pipelined）的延迟收尾等待 WAL 投影器结论的上限。期限内 finalizer
	// 不回源、不发布，只等投影器写入 Committed / Rejected / Indeterminate；超期后按回源结论收尾
	// （投影器停滞、DataEngine fence 等）。零值取 30s（RR-20260926-38）。
	FinalizeProjectionTimeout time.Duration
	WrapperCapacity           int
	WrapperIdleTTL            time.Duration
	// Versioned lock settings
	// LockKey 是版本锁键的前缀 / 锁身份（kit：remote_entity.lock_key），键为 lock:<LockKey>:<id> 与 lock:<LockKey>:<id>:fence。
	// 缺省 "e" 不带部署前缀且不能自动改（改了滚动发布的新旧节点会锁住不同身份，RR-20260924-25）；共用一个 Redis db 的
	// 多个部署应各自显式配置不同值，Cluster 下必须带 hash tag（RR-20260930-19 现状清单）。
	LockKey    string        // versioned lock key prefix, default "e"
	LockTTL    time.Duration // lock TTL, default 24h
	RetryCount int           // lock acquire retry count, default 5
	RetryDelay time.Duration // lock acquire retry interval, default 100ms

	// Unlock settings
	UnlockRetryCount    int           // unlock retry count, default 5
	UnlockRetryInterval time.Duration // unlock retry interval, default 100ms
	VersionTTL          time.Duration // version field TTL after unlock, default 24h

	// Lock timeout for operations (context timeout)
	OpTimeout time.Duration // default 30s
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		MaxWriteBatch:        100,
		SnapshotCacheShards:  64,
		SnapshotCacheEntries: 65536,
		SnapshotCacheBytes:   256 << 20,
		SnapshotCacheTTL:     30 * time.Second,
		SnapshotL2TTL:        5 * time.Minute,
		// O-M6-3：墓碑等 1 个副本、最多 50ms（同机房的异步复制通常亚毫秒到几毫秒确认）。
		SnapshotL2TombstoneWaitReplicas: 1,
		SnapshotL2TombstoneWaitTimeout:  50 * time.Millisecond,
		SnapshotInterestTTL:             30 * time.Second,
		SnapshotInterestKeys:            65536,
		SnapshotInterestSubs:            262144,
		MarkerCacheTTL:                  500 * time.Millisecond,
		SnapshotLoadTimeout:             2 * time.Second,
		SnapshotMaxWaiters:              256,
		MaxConcurrentWrites:             128,
		AsyncFinalizeCapacity:           4096,
		AsyncFinalizeWorkers:            16,
		TransactionTrackLimit:           65536,
		TransactionTrackTTL:             10 * time.Minute,
		FinalizeRetryInterval:           500 * time.Millisecond,
		FinalizeProjectionTimeout:       30 * time.Second,
		WrapperCapacity:                 65536,
		WrapperIdleTTL:                  5 * time.Minute,
		LockKey:                         "e",
		LockTTL:                         24 * time.Hour,
		RetryCount:                      5,
		RetryDelay:                      100 * time.Millisecond,
		UnlockRetryCount:                5,
		UnlockRetryInterval:             100 * time.Millisecond,
		VersionTTL:                      24 * time.Hour,
		OpTimeout:                       30 * time.Second,
	}
}
