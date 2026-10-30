package redis

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// redis.cluster_addrs 写成 YAML 列表时同样是 Cluster（不能读成空串退回 localhost 单机、跳过 hash tag 校验），逗号后的空格不进地址。
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
		if err := mods.ValidateClusterKeyPrefix(conn.ClusterAddrs, "mail", "roost:mail"); err == nil {
			t.Fatalf("cluster_addrs: %s: a key prefix without a hash tag passed the Cluster check", text)
		}
	}
}

// redis.db / redis.pool_size / redis.min_idle_conns 由 Redis Mod 与单实例锁的 SingletonStore 自己严格读取（维护者决定 A4）：
// 写成 8k、1.5、10s 这样的值在 Init 时点名报错，不被宽松 getter 读成 0；直接装配 Mod 的调用方（测试、工具）同样拿到错误。
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

// 生产环境要有 Redis，但 redis.addr 与 redis.cluster_addrs 任一即可（只配 Cluster 的生产配置不必再写不起作用的 addr）；这条跟着 Redis
// 配置的声明走，Redis Mod 与单实例锁的连接都受它约束。
func TestProductionRedisNeedsAnAddrOrClusterSeeds(t *testing.T) {
	production := func(body string) *viper.Viper {
		cfg := viper.New()
		cfg.SetConfigType("yaml")
		if err := cfg.ReadConfig(strings.NewReader("env: production\n" + body)); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	for name, body := range map[string]string{
		"addr":         "redis:\n  addr: 10.0.0.1:6379\n",
		"comma string": "redis:\n  cluster_addrs: 10.0.0.1:7000,10.0.0.2:7000\n",
		"yaml list":    "redis:\n  cluster_addrs: [10.0.0.1:7000, 10.0.0.2:7000]\n",
	} {
		if err := NewRedisMod().Init(production(body)); err != nil {
			t.Errorf("production redis with %s: %v", name, err)
		}
	}
	for name, body := range map[string]string{
		"nothing":       "",
		"empty addr":    "redis:\n  addr: \"\"\n",
		"only commas":   "redis:\n  cluster_addrs: \" , \"\n",
		"empty list":    "redis:\n  cluster_addrs: []\n",
		"blank entries": "redis:\n  cluster_addrs: [\" \", \"\"]\n",
	} {
		err := NewRedisMod().Init(production(body))
		if err == nil || !strings.Contains(err.Error(), "production requires redis.addr, redis.addr_env or redis.cluster_addrs") {
			t.Errorf("production redis with %s: Init = %v, want a missing-redis refusal", name, err)
		}
	}
}

// 连接与凭据从环境变量读（R11，规则见 docs/framework/guide/13-wiring.md §2.1）：redis.addr / redis.username / redis.password
// 各有 `_env` 伴随键；Redis Mod 与单实例锁的 SingletonStore 读同一份声明、走同一条规则。
func TestRedisAddressAndPasswordComeFromTheNamedEnvironmentVariables(t *testing.T) {
	t.Setenv("ROOST_TEST_REDIS_ADDR", "env-redis:6379")
	t.Setenv("ROOST_TEST_REDIS_PASSWORD", "s3cret-pw")
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader("redis:\n  addr: yaml-redis:6379\n  addr_env: ROOST_TEST_REDIS_ADDR\n  password_env: ROOST_TEST_REDIS_PASSWORD\n")); err != nil {
		t.Fatal(err)
	}
	mod := NewRedisMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.Addr != "env-redis:6379" || mod.cfg.Password != "s3cret-pw" {
		t.Fatalf("addr = %q, password from the variable = %v; want the values of the named variables", mod.cfg.Addr, mod.cfg.Password == "s3cret-pw")
	}
}

func TestRedisRefusesADeclaredButMissingEnvironmentVariable(t *testing.T) {
	t.Setenv("ROOST_TEST_REDIS_EMPTY", "")
	for _, key := range []string{"addr_env", "username_env", "password_env"} {
		cfg := viper.New()
		cfg.SetConfigType("yaml")
		if err := cfg.ReadConfig(strings.NewReader("redis:\n  addr: 127.0.0.1:1\n  " + key + ": ROOST_TEST_REDIS_EMPTY\n")); err != nil {
			t.Fatal(err)
		}
		if err := NewRedisMod().Init(cfg); err == nil || !strings.Contains(err.Error(), "redis."+key) || !strings.Contains(err.Error(), "ROOST_TEST_REDIS_EMPTY") {
			t.Fatalf("redis.%s: Init = %v; want a refusal naming redis.%s and the variable", key, err, key)
		}
		if _, err := SingletonStore(cfg); err == nil || !strings.Contains(err.Error(), "redis."+key) || !strings.Contains(err.Error(), "ROOST_TEST_REDIS_EMPTY") {
			t.Fatalf("redis.%s: SingletonStore = %v; want the same refusal", key, err)
		}
	}
}

// 生产要求有 Redis 地址：只写 redis.addr_env（地址全由部署注入）同样满足，不必再写一个不起作用的 redis.addr。
func TestProductionRedisAcceptsAnAddressFromTheEnvironment(t *testing.T) {
	t.Setenv("ROOST_TEST_REDIS_ADDR", "env-redis:6379")
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader("env: production\nredis:\n  addr_env: ROOST_TEST_REDIS_ADDR\n")); err != nil {
		t.Fatal(err)
	}
	mod := NewRedisMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatalf("production redis with only addr_env: %v", err)
	}
	if mod.cfg.Addr != "env-redis:6379" {
		t.Fatalf("addr = %q", mod.cfg.Addr)
	}
}

// ACL 用户名（下游 redis.username_env）进连接配置；与 cube 相同，写了用户名就要有口令。错误不带值。
func TestRedisUsernameComesFromTheEnvironmentAndNeedsAPassword(t *testing.T) {
	t.Setenv("ROOST_TEST_REDIS_USER", "roost")
	t.Setenv("ROOST_TEST_REDIS_PASSWORD", "s3cret-pw")
	read := func(body string) (*viper.Viper, *RedisMod, error) {
		cfg := viper.New()
		cfg.SetConfigType("yaml")
		if err := cfg.ReadConfig(strings.NewReader("redis:\n  addr: 127.0.0.1:1\n" + body)); err != nil {
			t.Fatal(err)
		}
		mod := NewRedisMod()
		return cfg, mod, mod.Init(cfg)
	}
	_, mod, err := read("  username_env: ROOST_TEST_REDIS_USER\n  password_env: ROOST_TEST_REDIS_PASSWORD\n")
	if err != nil {
		t.Fatal(err)
	}
	if mod.cfg.Username != "roost" || mod.cfg.Password != "s3cret-pw" {
		t.Fatalf("username = %q, password from the variable = %v", mod.cfg.Username, mod.cfg.Password == "s3cret-pw")
	}
	cfg, _, err := read("  username_env: ROOST_TEST_REDIS_USER\n")
	if err == nil || !strings.Contains(err.Error(), "redis.username requires redis.password") {
		t.Fatalf("username without a password: Init = %v", err)
	}
	if _, err := SingletonStore(cfg); err == nil || !strings.Contains(err.Error(), "redis.username requires redis.password") {
		t.Fatalf("username without a password: SingletonStore = %v", err)
	}
	// 只有口令（default 用户）照旧可用。
	if _, _, err := read("  password_env: ROOST_TEST_REDIS_PASSWORD\n"); err != nil {
		t.Fatalf("password only: %v", err)
	}
}
