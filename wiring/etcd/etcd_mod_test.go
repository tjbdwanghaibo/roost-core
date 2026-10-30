package etcd

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestEtcdRegistrationHasOneExplicitOwner(t *testing.T) {
	config := viper.New()
	config.Set("server_type", "game")
	config.Set("sid", 22)
	automatic := NewEtcdMod()
	if err := automatic.Init(config); err != nil {
		t.Fatal(err)
	}
	if automatic.serviceInfo == nil || automatic.serviceInfo.Sid != 22 {
		t.Fatal("default registration missing")
	}
	ingressOwns := NewEtcdMod(WithoutServiceRegistration())
	if err := ingressOwns.Init(config); err != nil {
		t.Fatal(err)
	}
	if ingressOwns.serviceInfo != nil {
		t.Fatal("two owners would register same Discovery")
	}
}

// 连接与凭据从环境变量读（R11，规则见 docs/framework/guide/13-wiring.md §2.1）：etcd.endpoints 的变量值按逗号分隔，
// 每项去掉两端空白（与配置里的写法相同）。
func TestEtcdEndpointsAndCredentialsComeFromTheNamedEnvironmentVariables(t *testing.T) {
	t.Setenv("ROOST_TEST_ETCD_ENDPOINTS", "etcd-a:2379, etcd-b:2379")
	t.Setenv("ROOST_TEST_ETCD_USER", "root")
	t.Setenv("ROOST_TEST_ETCD_PASSWORD", "s3cret-pw")
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 22)
	cfg.Set("etcd.endpoints", "yaml-etcd:2379")
	cfg.Set("etcd.endpoints_env", "ROOST_TEST_ETCD_ENDPOINTS")
	cfg.Set("etcd.username_env", "ROOST_TEST_ETCD_USER")
	cfg.Set("etcd.password_env", "ROOST_TEST_ETCD_PASSWORD")
	mod := NewEtcdMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if len(mod.cfg.Endpoints) != 2 || mod.cfg.Endpoints[0] != "etcd-a:2379" || mod.cfg.Endpoints[1] != "etcd-b:2379" {
		t.Fatalf("endpoints = %q, want [etcd-a:2379 etcd-b:2379]", mod.cfg.Endpoints)
	}
	if mod.cfg.Username != "root" || mod.cfg.Password != "s3cret-pw" {
		t.Fatalf("username = %q, password set = %v; want the values of the named variables", mod.cfg.Username, mod.cfg.Password != "")
	}
}

func TestEtcdRefusesADeclaredButMissingEnvironmentVariable(t *testing.T) {
	t.Setenv("ROOST_TEST_ETCD_EMPTY", "")
	t.Setenv("ROOST_TEST_ETCD_USER", "root")
	t.Setenv("ROOST_TEST_ETCD_PASSWORD", "s3cret-pw")
	for _, key := range []string{"endpoints_env", "username_env", "password_env"} {
		cfg := viper.New()
		cfg.Set("server_type", "game")
		cfg.Set("sid", 22)
		cfg.Set("etcd.username_env", "ROOST_TEST_ETCD_USER")
		cfg.Set("etcd.password_env", "ROOST_TEST_ETCD_PASSWORD")
		cfg.Set("etcd."+key, "ROOST_TEST_ETCD_EMPTY")
		err := NewEtcdMod().Init(cfg)
		if err == nil || !strings.Contains(err.Error(), "etcd."+key) || !strings.Contains(err.Error(), "ROOST_TEST_ETCD_EMPTY") {
			t.Fatalf("etcd.%s: Init = %v; want a refusal naming etcd.%s and the variable", key, err, key)
		}
		if strings.Contains(err.Error(), "s3cret-pw") {
			t.Fatalf("the refusal carries a credential: %v", err)
		}
	}
}

// 与 cube 相同：etcd 的用户名与口令同时写或都不写；错误不带值。下游无鉴权的部署写 `username_env: ""` / `password_env: ""` 清掉。
func TestEtcdUsernameAndPasswordGoTogether(t *testing.T) {
	t.Setenv("ROOST_TEST_ETCD_PASSWORD", "s3cret-pw")
	cfg := viper.New()
	cfg.Set("etcd.password_env", "ROOST_TEST_ETCD_PASSWORD")
	err := NewEtcdMod().Init(cfg)
	if err == nil || !strings.Contains(err.Error(), "etcd.username and etcd.password must be set together") || strings.Contains(err.Error(), "s3cret-pw") {
		t.Fatalf("password without a username: Init = %v", err)
	}
	cfg.Set("etcd.password_env", "")
	cfg.Set("etcd.username_env", "")
	if err := NewEtcdMod().Init(cfg); err != nil {
		t.Fatalf("cleared credentials: %v", err)
	}
}
