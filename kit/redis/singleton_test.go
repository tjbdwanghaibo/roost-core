package redis

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// 启用单实例锁的服务必须显式配置 Redis：opener 不沿用 RedisMod 的 localhost:6379 兜底。
func TestSingletonStoreRequiresAnExplicitRedisAddress(t *testing.T) {
	store, err := SingletonStore(viper.New())
	if err == nil {
		_ = store.Close()
		t.Fatal("SingletonStore opened without redis.addr or redis.cluster_addrs")
	}
	if !strings.Contains(err.Error(), "redis.addr") {
		t.Fatalf("error = %v, want it to name redis.addr", err)
	}
}

// RedisMod 仍保留 localhost:6379 的开发兜底（行为不变）。
func TestRedisModKeepsTheLocalhostDefault(t *testing.T) {
	mod := NewRedisMod()
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.Addr != "localhost:6379" {
		t.Fatalf("RedisMod addr = %q, want the localhost default", mod.cfg.Addr)
	}
}
