package nats

import (
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"

	"github.com/spf13/viper"
)

func TestJetStreamRPCConfigFromViper(t *testing.T) {
	cfg := viper.New()
	cfg.Set("nats.rpc.transport", "jetstream")
	cfg.Set("nats.rpc.request_stream", "RPC_REQ")
	cfg.Set("nats.rpc.response_stream", "RPC_RESP")
	cfg.Set("nats.rpc.ack_wait", "3s")
	cfg.Set("nats.rpc.max_deliver", 9)
	cfg.Set("nats.rpc.request_ttl", "11s")
	cfg.Set("nats.rpc.call_timeout", "12s")
	cfg.Set("nats.rpc.stream_max_age", "13m")
	cfg.Set("nats.rpc.duplicates", "14s")
	cfg.Set("nats.rpc.replicas", 2)
	cfg.Set("nats.rpc.max_bytes", int64(1024))

	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		t.Fatal(err)
	}
	got, enabled := settings.jetStreamRPC()
	if !enabled {
		t.Fatal("jetstream rpc config should be enabled")
	}
	if got.RequestStream != "RPC_REQ" || got.ResponseStream != "RPC_RESP" {
		t.Fatalf("streams = %q/%q", got.RequestStream, got.ResponseStream)
	}
	if got.AckWait != 3*time.Second || got.MaxDeliver != 9 || got.RequestTTL != 11*time.Second || got.CallTimeout != 12*time.Second {
		t.Fatalf("retry/timeouts = %+v", got)
	}
	if got.StreamMaxAge != 13*time.Minute || got.Duplicates != 14*time.Second || got.Replicas != 2 || got.MaxBytes != 1024 {
		t.Fatalf("stream options = %+v", got)
	}
}

func TestJetStreamRPCConfigFromViperDisabledByDefault(t *testing.T) {
	var settings config
	if err := app.LoadConfig(viper.New(), &settings); err != nil {
		t.Fatal(err)
	}
	got, enabled := settings.jetStreamRPC()
	if enabled {
		t.Fatalf("jetstream rpc config should be disabled by default: %+v", got)
	}
	if got != (bus.JetStreamRPCConfig{}) {
		t.Fatalf("disabled config = %+v", got)
	}
}

// 连接与凭据从环境变量读（R11，规则见 docs/framework/guide/13-wiring.md §2.1）：nats.url / nats.username / nats.password
// 各有 `_env` 伴随键；Init 与运维工具用的 Connect 读同一份声明、走同一条规则。
func TestNatsURLComesFromTheNamedEnvironmentVariable(t *testing.T) {
	t.Setenv("ROOST_TEST_NATS_URL", "nats://env-nats:4222")
	cfg := viper.New()
	cfg.Set("nats.url", "nats://yaml-nats:4222")
	cfg.Set("nats.url_env", "ROOST_TEST_NATS_URL")
	mod := NewNatsMod(nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.URL != "nats://env-nats:4222" {
		t.Fatalf("nats.url = %q, want the value of ROOST_TEST_NATS_URL", mod.cfg.URL)
	}
}

func TestNatsRefusesADeclaredButMissingEnvironmentVariable(t *testing.T) {
	t.Setenv("ROOST_TEST_NATS_EMPTY", "")
	t.Setenv("ROOST_TEST_NATS_USER", "roost")
	t.Setenv("ROOST_TEST_NATS_PASSWORD", "s3cret-pw")
	for _, key := range []string{"url_env", "username_env", "password_env"} {
		cfg := viper.New()
		cfg.Set("nats.url", "nats://127.0.0.1:1")
		cfg.Set("nats.username_env", "ROOST_TEST_NATS_USER")
		cfg.Set("nats.password_env", "ROOST_TEST_NATS_PASSWORD")
		cfg.Set("nats."+key, "ROOST_TEST_NATS_EMPTY")
		err := NewNatsMod(nil).Init(cfg)
		if err == nil || !strings.Contains(err.Error(), "nats."+key) || !strings.Contains(err.Error(), "ROOST_TEST_NATS_EMPTY") {
			t.Fatalf("nats.%s: Init = %v; want a refusal naming nats.%s and the variable", key, err, key)
		}
		if strings.Contains(err.Error(), "s3cret-pw") {
			t.Fatalf("the refusal carries a credential: %v", err)
		}
		if _, err := Connect(cfg); err == nil || !strings.Contains(err.Error(), "nats."+key) {
			t.Fatalf("nats.%s: Connect = %v; want the same refusal before dialing", key, err)
		}
	}
}

// 账号单独写（下游 nats.username_env / password_env）进连接配置；与 cube 相同用户名与口令同时写，nats.url 已带 userinfo 时
// 不能再写用户名。错误不带地址与口令；运维工具的 ServerURL 与 Init 读到同一个地址。
func TestNatsAccountComesFromTheEnvironmentAndMustNotBeAmbiguous(t *testing.T) {
	t.Setenv("ROOST_TEST_NATS_URL", "nats://env-nats:4222")
	t.Setenv("ROOST_TEST_NATS_USER", "roost")
	t.Setenv("ROOST_TEST_NATS_PASSWORD", "s3cret-pw")
	cfg := viper.New()
	cfg.Set("nats.url_env", "ROOST_TEST_NATS_URL")
	cfg.Set("nats.username_env", "ROOST_TEST_NATS_USER")
	cfg.Set("nats.password_env", "ROOST_TEST_NATS_PASSWORD")
	mod := NewNatsMod(nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.Username != "roost" || mod.cfg.Password != "s3cret-pw" {
		t.Fatalf("username = %q, password from the variable = %v", mod.cfg.Username, mod.cfg.Password == "s3cret-pw")
	}
	if url, err := ServerURL(cfg); err != nil || url != "nats://env-nats:4222" {
		t.Fatalf("ServerURL = %q, %v", url, err)
	}

	t.Setenv("ROOST_TEST_NATS_URL_WITH_USER", "nats://other:s3cret-url-pw@env-nats:4222")
	for name, tc := range map[string]struct {
		set  map[string]string
		want string
	}{
		"username without password":  {map[string]string{"nats.username_env": "ROOST_TEST_NATS_USER"}, "nats.username and nats.password must be set together"},
		"credentials in both places": {map[string]string{"nats.url_env": "ROOST_TEST_NATS_URL_WITH_USER", "nats.username_env": "ROOST_TEST_NATS_USER", "nats.password_env": "ROOST_TEST_NATS_PASSWORD"}, "nats.url already carries credentials"},
	} {
		cfg := viper.New()
		for key, value := range tc.set {
			cfg.Set(key, value)
		}
		err := NewNatsMod(nil).Init(cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: Init = %v; want %q", name, err, tc.want)
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "env-nats") {
			t.Fatalf("%s: the refusal carries the URL or a credential: %v", name, err)
		}
	}
}
