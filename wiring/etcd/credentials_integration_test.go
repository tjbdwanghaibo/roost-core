//go:build integration

package etcd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// 真实鉴权 etcd 上的 R11 验收：用户名与口令只从环境变量来（etcd.username_env / password_env），经正式的
// Init → Provide → Start（建租约、写服务注册，需要鉴权）成功；同一实例不带账号时 Start 失败（环境确实开了鉴权）。
//
//	ROOST_IT_AUTH_ETCD_ENDPOINTS=127.0.0.1:<端口>  ROOST_IT_AUTH_ETCD_USERNAME=root  ROOST_IT_AUTH_ETCD_PASSWORD=<口令>
//
// （etcdctl user add root、auth enable 之后）。注册键写在本用例独有的前缀下，Stop 撤销租约后随之删除。
func TestRealEtcdModReadsCredentialsFromTheEnvironment(t *testing.T) {
	endpoints := os.Getenv("ROOST_IT_AUTH_ETCD_ENDPOINTS")
	if endpoints == "" || os.Getenv("ROOST_IT_AUTH_ETCD_USERNAME") == "" {
		t.Skip("ROOST_IT_AUTH_ETCD_ENDPOINTS / _USERNAME / _PASSWORD are not set; run against a private etcd with auth enabled")
	}
	prefix := fmt.Sprintf("/r11-credentials-%d/services/", time.Now().UnixNano())
	config := func(credentials bool) *viper.Viper {
		cfg := viper.New()
		cfg.Set("server_type", "game")
		cfg.Set("sid", 1)
		cfg.Set("etcd.endpoints", endpoints)
		cfg.Set("etcd.service_prefix", prefix)
		if credentials {
			cfg.Set("etcd.username_env", "ROOST_IT_AUTH_ETCD_USERNAME")
			cfg.Set("etcd.password_env", "ROOST_IT_AUTH_ETCD_PASSWORD")
		}
		return cfg
	}
	start := func(cfg *viper.Viper) error {
		mod := NewEtcdMod()
		if err := mod.Init(cfg); err != nil {
			return err
		}
		if err := mod.Provide(app.NewRegistry(cfg)); err != nil {
			return err
		}
		defer func() { _ = mod.StopWithContext(context.Background()) }()
		return mod.Start()
	}
	if err := start(config(false)); err == nil {
		t.Fatal("an unauthenticated client registered: the cluster does not enforce authentication, the test would pass vacuously")
	}
	if err := start(config(true)); err != nil {
		t.Fatalf("credentials from the environment: %v", err)
	}
	password := os.Getenv("ROOST_IT_AUTH_ETCD_PASSWORD")
	t.Setenv("ROOST_IT_AUTH_ETCD_PASSWORD", password+"-wrong")
	err := start(config(true))
	if err == nil {
		t.Fatal("a wrong password from the environment still registered")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("the failure carries the password: %v", err)
	}
}
