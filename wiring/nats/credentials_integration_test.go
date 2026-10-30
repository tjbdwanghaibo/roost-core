//go:build integration

package nats

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// 真实鉴权 NATS 上的 R11 验收：账号只从环境变量来（nats.username_env / password_env），经正式的 Init → Provide（建连接）→
// Start 连通；同一实例不带账号时 Provide 失败（环境确实开了鉴权）；运维工具的 Connect 走同一规则。失败的错误不带口令。
//
//	ROOST_IT_AUTH_NATS_URL=nats://127.0.0.1:<端口>  ROOST_IT_AUTH_NATS_USERNAME=<用户>  ROOST_IT_AUTH_NATS_PASSWORD=<口令>
func TestRealNatsModReadsCredentialsFromTheEnvironment(t *testing.T) {
	url := os.Getenv("ROOST_IT_AUTH_NATS_URL")
	if url == "" || os.Getenv("ROOST_IT_AUTH_NATS_USERNAME") == "" {
		t.Skip("ROOST_IT_AUTH_NATS_URL / _USERNAME / _PASSWORD are not set; run against a private nats-server started with --user / --pass")
	}
	config := func(credentials bool) *viper.Viper {
		cfg := viper.New()
		cfg.Set("server_type", "game")
		cfg.Set("sid", 1)
		cfg.Set("nats.url", url)
		cfg.Set("nats.prefix", "r11credentials")
		if credentials {
			cfg.Set("nats.username_env", "ROOST_IT_AUTH_NATS_USERNAME")
			cfg.Set("nats.password_env", "ROOST_IT_AUTH_NATS_PASSWORD")
		}
		return cfg
	}
	start := func(cfg *viper.Viper) error {
		mod := NewNatsMod(nil)
		if err := mod.Init(cfg); err != nil {
			return err
		}
		defer func() { _ = mod.StopWithContext(context.Background()) }()
		if err := mod.Provide(app.NewRegistry(cfg)); err != nil {
			return err
		}
		return mod.Start()
	}
	if err := start(config(false)); err == nil {
		t.Fatal("an unauthenticated client connected: the server does not enforce authentication, the test would pass vacuously")
	}
	if err := start(config(true)); err != nil {
		t.Fatalf("credentials from the environment: %v", err)
	}
	connection, err := Connect(config(true))
	if err != nil {
		t.Fatalf("Connect with credentials from the environment: %v", err)
	}
	_ = connection.Close(context.Background())
	password := os.Getenv("ROOST_IT_AUTH_NATS_PASSWORD")
	t.Setenv("ROOST_IT_AUTH_NATS_PASSWORD", password+"-wrong")
	err = start(config(true))
	if err == nil {
		t.Fatal("a wrong password from the environment still connected")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("the failure carries the password: %v", err)
	}
}
