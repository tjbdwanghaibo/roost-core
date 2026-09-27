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
	SnapshotL2KeyPrefix  string
	SnapshotInterestTTL  time.Duration
	SnapshotInterestKeys int
	SnapshotInterestSubs int
	MarkerCacheTTL       time.Duration
	SnapshotLoadTimeout  time.Duration
	SnapshotMaxWaiters   int
	// MaxConcurrentWrites 限制从 Prepare 到真正释放的写批次，包括后台收尾。
	// 不等待额度；满额直接 ErrRemoteOverloaded。零值沿用收尾容量，默认配置为 128。
	MaxConcurrentWrites   int
	AsyncFinalizeCapacity int
	AsyncFinalizeWorkers  int
	TransactionTrackLimit int
	TransactionTrackTTL   time.Duration
	FinalizeRetryInterval time.Duration
	// FinalizeProjectionTimeout 是 Durability 1/2 的延迟收尾等待 WAL 投影器结论的上限。期限内 finalizer
	// 不回源、不发布，只等投影器写入 Committed / Rejected / Indeterminate；超期后按回源结论收尾
	// （投影器停滞、DataEngine fence 等）。零值取 30s（RR-20260926-38）。
	FinalizeProjectionTimeout time.Duration
	WrapperCapacity           int
	WrapperIdleTTL            time.Duration
	// Versioned lock settings
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
		MaxWriteBatch:             100,
		SnapshotCacheShards:       64,
		SnapshotCacheEntries:      65536,
		SnapshotCacheBytes:        256 << 20,
		SnapshotCacheTTL:          30 * time.Second,
		SnapshotL2TTL:             5 * time.Minute,
		SnapshotInterestTTL:       30 * time.Second,
		SnapshotInterestKeys:      65536,
		SnapshotInterestSubs:      262144,
		MarkerCacheTTL:            500 * time.Millisecond,
		SnapshotLoadTimeout:       2 * time.Second,
		SnapshotMaxWaiters:        256,
		MaxConcurrentWrites:       128,
		AsyncFinalizeCapacity:     4096,
		AsyncFinalizeWorkers:      16,
		TransactionTrackLimit:     65536,
		TransactionTrackTTL:       10 * time.Minute,
		FinalizeRetryInterval:     500 * time.Millisecond,
		FinalizeProjectionTimeout: 30 * time.Second,
		WrapperCapacity:           65536,
		WrapperIdleTTL:            5 * time.Minute,
		LockKey:                   "e",
		LockTTL:                   24 * time.Hour,
		RetryCount:                5,
		RetryDelay:                100 * time.Millisecond,
		UnlockRetryCount:          5,
		UnlockRetryInterval:       100 * time.Millisecond,
		VersionTTL:                24 * time.Hour,
		OpTimeout:                 30 * time.Second,
	}
}
