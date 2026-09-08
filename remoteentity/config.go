package remoteentity

import "time"

// Config holds configuration for the remote entity module.
type Config struct {
	MaxWriteBatch         int
	SnapshotCacheShards   int
	SnapshotCacheEntries  int
	SnapshotCacheBytes    int64
	SnapshotCacheTTL      time.Duration
	SnapshotL2TTL         time.Duration
	SnapshotInterestTTL   time.Duration
	SnapshotInterestKeys  int
	SnapshotInterestSubs  int
	MarkerCacheTTL        time.Duration
	SnapshotLoadTimeout   time.Duration
	SnapshotMaxWaiters    int
	AsyncFinalizeCapacity int
	AsyncFinalizeWorkers  int
	TransactionTrackLimit int
	TransactionTrackTTL   time.Duration
	FinalizeRetryInterval time.Duration
	WrapperCapacity       int
	WrapperIdleTTL        time.Duration
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
		MaxWriteBatch:         100,
		SnapshotCacheShards:   64,
		SnapshotCacheEntries:  65536,
		SnapshotCacheBytes:    256 << 20,
		SnapshotCacheTTL:      30 * time.Second,
		SnapshotL2TTL:         5 * time.Minute,
		SnapshotInterestTTL:   30 * time.Second,
		SnapshotInterestKeys:  65536,
		SnapshotInterestSubs:  262144,
		MarkerCacheTTL:        500 * time.Millisecond,
		SnapshotLoadTimeout:   2 * time.Second,
		SnapshotMaxWaiters:    256,
		AsyncFinalizeCapacity: 4096,
		AsyncFinalizeWorkers:  16,
		TransactionTrackLimit: 65536,
		TransactionTrackTTL:   10 * time.Minute,
		FinalizeRetryInterval: 500 * time.Millisecond,
		WrapperCapacity:       65536,
		WrapperIdleTTL:        5 * time.Minute,
		LockKey:               "e",
		LockTTL:               24 * time.Hour,
		RetryCount:            5,
		RetryDelay:            100 * time.Millisecond,
		UnlockRetryCount:      5,
		UnlockRetryInterval:   100 * time.Millisecond,
		VersionTTL:            24 * time.Hour,
		OpTimeout:             30 * time.Second,
	}
}
