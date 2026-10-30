//go:build integration

package redis

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// 真实鉴权 Redis 上的 R11 验收：ACL 用户名与口令只从环境变量来（redis.username_env / password_env），经正式的
// Init → Provide → Start（PING）连通；同一实例不带账号时 Start 失败（环境确实开了鉴权）；单实例锁的 SingletonStore 走同一规则。
//
//	ROOST_IT_AUTH_REDIS_ADDR=127.0.0.1:<端口>  ROOST_IT_AUTH_REDIS_USERNAME=<ACL 用户>  ROOST_IT_AUTH_REDIS_PASSWORD=<口令>
func TestRealRedisModReadsCredentialsFromTheEnvironment(t *testing.T) {
	addr := os.Getenv("ROOST_IT_AUTH_REDIS_ADDR")
	if addr == "" || os.Getenv("ROOST_IT_AUTH_REDIS_USERNAME") == "" {
		t.Skip("ROOST_IT_AUTH_REDIS_ADDR / _USERNAME / _PASSWORD are not set; run against a private redis-server with an ACL user")
	}
	config := func(credentials bool) *viper.Viper {
		cfg := viper.New()
		cfg.Set("redis.addr", addr)
		if credentials {
			cfg.Set("redis.username_env", "ROOST_IT_AUTH_REDIS_USERNAME")
			cfg.Set("redis.password_env", "ROOST_IT_AUTH_REDIS_PASSWORD")
		}
		return cfg
	}
	start := func(cfg *viper.Viper) error {
		mod := NewRedisMod()
		if err := mod.Init(cfg); err != nil {
			return err
		}
		if err := mod.Provide(app.NewRegistry(cfg)); err != nil {
			return err
		}
		defer func() { _ = mod.StopWithContext(t.Context()) }()
		return mod.Start()
	}
	if err := start(config(false)); err == nil {
		t.Fatal("an unauthenticated client got PONG: the instance does not enforce authentication, the test would pass vacuously")
	}
	if err := start(config(true)); err != nil {
		t.Fatalf("credentials from the environment: %v", err)
	}
	store, err := SingletonStore(config(true))
	if err != nil {
		t.Fatalf("SingletonStore with credentials from the environment: %v", err)
	}
	_ = store.Close()
	password := os.Getenv("ROOST_IT_AUTH_REDIS_PASSWORD")
	t.Setenv("ROOST_IT_AUTH_REDIS_PASSWORD", password+"-wrong")
	err = start(config(true))
	if err == nil {
		t.Fatal("a wrong password from the environment still connected")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("the failure carries the password: %v", err)
	}
}
