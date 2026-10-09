package remoteentity

import (
	"strings"
	"testing"
	"time"
)

// RR-20261006-71：Assemble 与 NewSnapshotClient 用同一套快照段校验。
//
// 修前 NewManager 用不校验的 newSnapshotClient，validateSnapshotClientConfig 只在只读方的 NewSnapshotClient
// 路径执行：SnapshotInterestTTL = 0 时每次续租 ExpiresAt = now，兴趣表一律拒绝，owner 永不推送；
// SnapshotL2TTL = 0 时墓碑不落地、O5 过滤关闭。只读装配会被拒绝的同一组配置，owner 装配静默接受。
func TestAssembleRejectsTheSnapshotConfigNewSnapshotClientRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"interest_ttl_zero", func(c *Config) { c.SnapshotInterestTTL = 0 }, "snapshot_interest_ttl"},
		{"load_timeout_zero", func(c *Config) { c.SnapshotLoadTimeout = 0 }, "snapshot_load_timeout"},
		{"l2_ttl_zero", func(c *Config) { c.SnapshotL2TTL = 0 }, "snapshot_l2_ttl"},
		{"cache_ttl_negative", func(c *Config) { c.SnapshotCacheTTL = -1 }, "snapshot cache ttl"},
		{"per_consumer_over_subs", func(c *Config) { c.SnapshotInterestSubs = 4; c.SnapshotInterestPerConsumer = 5 }, "snapshot_interest_per_consumer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(cfg)
			if _, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 7, L2: NewSnapshotL2Store(assembleRedis{}, time.Minute)}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("precondition: NewSnapshotClient should reject with %q, got %v", tc.want, err)
			}
			backend := &atomicTestBackend{newRemoteTestLoader()}
			assembly, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: backend}, cfg, 7, MongoBackendConfig{})
			if err == nil {
				t.Fatalf("Assemble accepted a snapshot config NewSnapshotClient rejects (assembly=%v)", assembly != nil)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}
	// 缺省配置照常通过。
	if _, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, DefaultConfig(), 7, MongoBackendConfig{}); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
}
