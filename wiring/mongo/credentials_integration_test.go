//go:build integration

package mongo

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 真实鉴权 Mongo 上的 R11 验收：账号只从环境变量来（mongo.username_env / password_env + auth_source），经正式的
// Init → Provide → Start 连上后能写（Mongo 的 ping / hello 不要求鉴权，所以用写入证明账号确实生效）；同一实例不带账号时
// 写入被拒（环境确实开了鉴权，不是空过）；变量里的口令错时 Start 失败。
//
// 需要一个开了 --auth 的私有 mongod（单机即可，测试写 mongo.require_replica_set=false），在 admin 库有一个可读写任意库的用户：
//
//	ROOST_IT_AUTH_MONGO_URI=mongodb://127.0.0.1:<端口>   （不带账号）
//	ROOST_IT_AUTH_MONGO_USERNAME=<用户>  ROOST_IT_AUTH_MONGO_PASSWORD=<口令>
func TestRealMongoModReadsCredentialsFromTheEnvironment(t *testing.T) {
	uri := os.Getenv("ROOST_IT_AUTH_MONGO_URI")
	if uri == "" || os.Getenv("ROOST_IT_AUTH_MONGO_USERNAME") == "" {
		t.Skip("ROOST_IT_AUTH_MONGO_URI / _USERNAME / _PASSWORD are not set; run against a private mongod started with --auth")
	}
	config := func(credentials bool) *viper.Viper {
		cfg := viper.New()
		cfg.Set("mongo.uri", uri)
		cfg.Set("mongo.require_replica_set", false)
		cfg.Set("mongo.connect_timeout", 3*time.Second)
		if credentials {
			cfg.Set("mongo.username_env", "ROOST_IT_AUTH_MONGO_USERNAME")
			cfg.Set("mongo.password_env", "ROOST_IT_AUTH_MONGO_PASSWORD")
			cfg.Set("mongo.auth_source", "admin")
		}
		return cfg
	}
	write := func(cfg *viper.Viper) error {
		mod := NewMongoMod()
		if err := mod.Init(cfg); err != nil {
			return fmt.Errorf("init: %w", err)
		}
		registry := app.NewRegistry(cfg)
		if err := mod.Provide(registry); err != nil {
			return fmt.Errorf("provide: %w", err)
		}
		defer func() { _ = mod.StopWithContext(context.Background()) }()
		if err := mod.Start(); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		client, _ := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		db := client.Database(fmt.Sprintf("r11_credentials_%d", time.Now().UnixNano()))
		if _, err := db.Collection("probe").InsertOne(ctx, map[string]any{"_id": "r11"}); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return db.Drop(ctx)
	}
	if err := write(config(false)); err == nil {
		t.Fatal("an unauthenticated client could write: the instance does not enforce authentication, the test would pass vacuously")
	}
	if err := write(config(true)); err != nil {
		t.Fatalf("credentials from the environment: %v", err)
	}
	password := os.Getenv("ROOST_IT_AUTH_MONGO_PASSWORD")
	t.Setenv("ROOST_IT_AUTH_MONGO_PASSWORD", password+"-wrong")
	err := write(config(true))
	if err == nil {
		t.Fatal("a wrong password from the environment still wrote")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("the failure carries the password: %v", err)
	}
}
