package app

// RR-20261006-28：生产校验要求依赖 Redis 的服务配置了 Redis，但只认 redis.addr。只配
// redis.cluster_addrs（Redis Mod、单实例锁、各服务的 hash tag 校验都按它切 Cluster）的生产配置被拒：
// “production game requires redis.addr”——想让它启动只能再写一个 addr，而那个 addr 被 Cluster 配置覆盖、
// 不起作用，正是 NC-192 要删掉的“写上只为让校验放行”的设置。cluster_addrs 可以是逗号串或 YAML 列表。

import (
	"strings"
	"testing"
)

func TestProductionServicesOnARedisClusterPassWithoutRedisAddr(t *testing.T) {
	for _, serverType := range []string{"game", "instance", "account", "global", "match_group"} {
		for name, seeds := range map[string]any{
			"comma string": "10.0.0.1:7000,10.0.0.2:7000",
			"yaml list":    []any{"10.0.0.1:7000", "10.0.0.2:7000"},
		} {
			cfg := productionServiceConfig(serverType)
			cfg.Set("redis.addr", "")
			cfg.Set("redis.cluster_addrs", seeds)
			if err := ValidateServiceConfig(cfg); err != nil {
				t.Errorf("production %s on a Redis Cluster (%s): %v", serverType, name, err)
			}
		}
	}
}

func TestProductionServicesStillNeedSomeRedis(t *testing.T) {
	for name, seeds := range map[string]any{
		"empty string":  "",
		"only commas":   " , ",
		"empty list":    []any{},
		"blank entries": []any{" ", ""},
	} {
		cfg := productionServiceConfig("game")
		cfg.Set("redis.addr", "")
		cfg.Set("redis.cluster_addrs", seeds)
		err := ValidateServiceConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "production game requires redis.addr or redis.cluster_addrs") {
			t.Errorf("production game with cluster_addrs %s and no addr: ValidateServiceConfig error = %v, want a missing-redis error", name, err)
		}
	}
}
