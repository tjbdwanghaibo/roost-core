package redis

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

// RR-20261005-NC-190：redis.cluster_addrs 写成 YAML 列表时同样是 Cluster，逗号后的空格不进地址。
// 旧行为：列表读成空串，Redis Mod 退回 localhost:6379 单机、服务 Mod 的 Cluster hash tag 校验被
// 跳过；`a:1, b:2` 拆出带前导空格的 " b:2"。
func TestClusterAddrsAcceptAYAMLListAndTrimEntries(t *testing.T) {
	for _, text := range []string{"a:1,b:2", "a:1, b:2", "[a:1, b:2]", "\n    - a:1\n    - b:2"} {
		cfg := viper.New()
		cfg.SetConfigType("yaml")
		if err := cfg.ReadConfig(strings.NewReader("redis:\n  cluster_addrs: " + text + "\n")); err != nil {
			t.Fatal(err)
		}
		conn, err := redisConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !conn.IsCluster() || len(conn.ClusterAddrs) != 2 || conn.ClusterAddrs[0] != "a:1" || conn.ClusterAddrs[1] != "b:2" {
			t.Fatalf("cluster_addrs: %s -> cluster=%v addrs=%q, want Cluster [a:1 b:2]", text, conn.IsCluster(), conn.ClusterAddrs)
		}
		if err := mods.ValidateClusterKeyPrefix(cfg, "mail", "roost:mail"); err == nil {
			t.Fatalf("cluster_addrs: %s: a key prefix without a hash tag passed the Cluster check", text)
		}
	}
}

// A4 留项（维护者决定 A4，REFACTOR-2026-10-05-strict-config-reads）：redis.db / redis.pool_size /
// redis.min_idle_conns 由 Redis Mod 与单实例锁的 SingletonStore 自己严格读取，写成 8k、1.5、10s 这样的值
// 在 Init 时点名报错，不再被 viper 的宽松 getter 读成 0（db 0、连接池取默认）。旧行为只靠 App 启动时的
// ValidateServiceConfig 兜底，直接装配 Mod 的调用方（测试、工具）读到的是 0。
func TestRedisIntegerKeysAreReadStrictly(t *testing.T) {
	for _, key := range []string{"db", "pool_size", "min_idle_conns"} {
		for _, bad := range []string{"8k", "1.5", "10s"} {
			cfg := viper.New()
			cfg.SetConfigType("yaml")
			if err := cfg.ReadConfig(strings.NewReader("redis:\n  addr: 127.0.0.1:6379\n  " + key + ": " + bad + "\n")); err != nil {
				t.Fatal(err)
			}
			err := NewRedisMod().Init(cfg)
			if err == nil || !strings.Contains(err.Error(), "redis."+key) {
				t.Fatalf("redis.%s: %s: Init returned %v, want a refusal naming redis.%s", key, bad, err, key)
			}
			if _, err := SingletonStore(cfg); err == nil || !strings.Contains(err.Error(), "redis."+key) {
				t.Fatalf("redis.%s: %s: SingletonStore returned %v, want a refusal naming redis.%s", key, bad, err, key)
			}
		}
	}
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader("redis:\n  addr: 127.0.0.1:6379\n  db: 2\n  pool_size: 16\n  min_idle_conns: 3\n")); err != nil {
		t.Fatal(err)
	}
	mod := NewRedisMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.DB != 2 || mod.cfg.PoolSize != 16 || mod.cfg.MinIdleConns != 3 {
		t.Fatalf("db / pool_size / min_idle_conns = %d / %d / %d, want 2 / 16 / 3", mod.cfg.DB, mod.cfg.PoolSize, mod.cfg.MinIdleConns)
	}
}
