package storeservice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
)

func yamlConfig(t *testing.T, text string) *viper.Viper {
	t.Helper()
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader(text)); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// 权威 Redis 的连接池要容下 Store Service 的分区循环；与 saga Mod 同进程时再加上 saga 的并发。配置检查在任何 Mod Init 之前
// 拒绝，并点名 redis_authority.pool_size。
func TestStoreServiceRejectsSmallAuthorityPool(t *testing.T) {
	base := "sid: 1\nserver_type: game\nredis_authority:\n  addr: 127.0.0.1:6379\n  pool_size: %s\nsaga:\n  redis:\n    key_prefix: test:saga\n"
	check := func(pool, extra string, all ...app.Mod) error {
		return app.CheckConfig(yamlConfig(t, strings.Replace(base, "%s", pool, 1)+extra), all...)
	}
	// saga 28（4+4+8+8+4）+ 16 个分区 = 44。
	if err := check("44", "", kitsaga.NewMod(), kitredis.NewAuthorityMod(), NewMod()); err != nil {
		t.Fatalf("pool 44 with saga and the store service refused: %v", err)
	}
	if err := check("43", "", kitsaga.NewMod(), kitredis.NewAuthorityMod(), NewMod()); err == nil || !strings.Contains(err.Error(), "redis_authority.pool_size") || !strings.Contains(err.Error(), "44") {
		t.Fatalf("pool 43 under 44 = %v, want a refusal naming redis_authority.pool_size", err)
	}
	if err := check("15", "", kitredis.NewAuthorityMod(), NewMod()); err == nil || !strings.Contains(err.Error(), "16") {
		t.Fatalf("a store-service-only process with pool 15 under 16 partitions = %v", err)
	}
	if err := check("15", "store_service:\n  max_partitions: 4\n", kitredis.NewAuthorityMod(), NewMod()); err != nil {
		t.Fatalf("max_partitions 4 under pool 15 refused: %v", err)
	}
}

// Store Service Mod 的生命周期：Init 读 store_service.* 与 saga.*，Provide 装配 saga 投影与运行器并登记健康检查，Start 起
// 分区循环，Stop 在 ctx 内等它们退出；没有暂停分区时健康 OK。
func TestStoreServiceModLifecycle(t *testing.T) {
	addr := redistest.AOFAddr(t)
	cfg := yamlConfig(t, "sid: 7\nserver_type: game\nstore_service:\n  lease_duration: 1s\n  block: 20ms\n")
	cfg.Set("saga.redis.key_prefix", redistest.Prefix(t, addr, "storeservicemod"))
	cfg.Set("saga.redis.partitions", 2)
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModMongo, mongotest.NewClient()); err != nil {
		t.Fatal(err)
	}
	authority, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	if err := registry.Register(mods.ModRedisAuthority, authority); err != nil {
		t.Fatal(err)
	}
	mod := NewMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mod.settings.StoreService.Owner, "store-7-") {
		t.Fatalf("owner = %q, want store-<sid>-<id>", mod.settings.StoreService.Owner)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	if got := mod.checkHealth(context.Background()); got.Status != health.StatusOK {
		t.Fatalf("health after start = %+v", got)
	}
	if mod.Runner().Concurrency() != 2 {
		t.Fatalf("concurrency = %d, want the 2 partitions", mod.Runner().Concurrency())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mod.StopWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mod.StopWithContext(ctx); err != nil {
		t.Fatalf("second stop = %v", err)
	}
}
