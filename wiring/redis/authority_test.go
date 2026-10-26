package redis

import (
	"context"
	"errors"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func startAuthorityMod(t *testing.T, addr string) (*AuthorityMod, *app.Registry, error) {
	t.Helper()
	cfg := viper.New()
	cfg.Set("redis_authority.addr", addr)
	registry := app.NewRegistry(cfg) // health 是注册表的内建能力
	mod := NewAuthorityMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })
	return mod, registry, mod.Start()
}

// 权威 Redis Mod 的启动探针：满足部署契约（AOF always、noeviction、单主）的实例启动成功并发布能力；appendfsync 不是 always
// 的实例拒绝启动，错误点名契约。
func TestAuthorityModRefusesAnInstanceThatBreaksTheContract(t *testing.T) {
	server := redistest.StartAOF(t)
	_, registry, err := startAuthorityMod(t, server.Addr)
	if err != nil {
		t.Fatalf("a conforming instance was refused: %v", err)
	}
	if client, ok := app.Lookup[fredis.IRedis](registry, mods.ModRedisAuthority); !ok || client == nil {
		t.Fatal("the authority capability was not published")
	}
	raw := goredis.NewClient(&goredis.Options{Addr: server.Addr})
	defer raw.Close()
	if err := raw.ConfigSet(context.Background(), "appendfsync", "everysec").Err(); err != nil {
		t.Fatal(err)
	}
	_, _, err = startAuthorityMod(t, server.Addr)
	if !errors.Is(err, fredis.ErrAuthorityContract) || !strings.Contains(err.Error(), "appendfsync") {
		t.Fatalf("Start on appendfsync everysec = %v, want the contract refusal naming appendfsync", err)
	}
}

// redis_authority.* 的声明：生产必须写 addr；连接池缺省 64。
func TestAuthorityConfigDeclaration(t *testing.T) {
	settings, err := LoadAuthorityConfig(viper.New())
	if err != nil {
		t.Fatal(err)
	}
	if settings.PoolSize != 64 || settings.MinIdleConns != 8 || settings.connection().Addr != "localhost:6379" {
		t.Fatalf("defaults = %+v", settings)
	}
	if err := (&AuthorityConfig{}).ValidateConfig(true); err == nil || !strings.Contains(err.Error(), "redis_authority.addr") {
		t.Fatalf("production without redis_authority.addr = %v", err)
	}
	if mod := NewAuthorityMod(); mod.StopWithContext(context.Background()) != nil {
		t.Fatal("stopping an unstarted mod failed")
	}
}
