package app

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestValidateServiceConfigRejectsInvalidJetStreamRPC(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 2001)
	cfg.Set("nats.rpc.transport", "jetstream")
	cfg.Set("nats.rpc.call_timeout", "0s")

	err := ValidateServiceConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "nats.rpc.call_timeout") {
		t.Fatalf("ValidateServiceConfig error = %v", err)
	}
}

func TestValidateServiceConfigRejectsOpsAdminWithoutToken(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 2001)
	cfg.Set("ops.admin_enabled", true)

	err := ValidateServiceConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "ops.admin_token") {
		t.Fatalf("ValidateServiceConfig error = %v", err)
	}
}

func TestValidateServiceConfigAcceptsMinimalConfig(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 2001)

	if err := ValidateServiceConfig(cfg); err != nil {
		t.Fatalf("ValidateServiceConfig: %v", err)
	}
}

func TestValidateServiceConfigRejectsUnsafeProductionAccountConfig(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "account")
	cfg.Set("sid", 9201)
	cfg.Set("env", "production")
	cfg.Set("redis.addr", "127.0.0.1:6379")
	cfg.Set("account.session_secret", "dev-session-secret")

	err := ValidateServiceConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "account.session_secret") {
		t.Fatalf("ValidateServiceConfig error = %v, want account.session_secret", err)
	}
}

func TestValidateServiceConfigRejectsProductionAccountWithoutRedis(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "account")
	cfg.Set("sid", 9201)
	cfg.Set("env", "production")
	cfg.Set("account.session_secret", "session-secret")
	cfg.Set("redis.addr", "")

	err := ValidateServiceConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "redis.addr") {
		t.Fatalf("ValidateServiceConfig error = %v, want redis.addr", err)
	}
}

func TestValidateServiceConfigRejectsUnsafeProductionPlatformConfig(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "platform")
	cfg.Set("sid", 9001)
	cfg.Set("env", "production")
	cfg.Set("platform.session_secret", "dev-session-secret")
	cfg.Set("platform.payment_secret", "")

	err := ValidateServiceConfig(cfg)
	if err == nil {
		t.Fatal("ValidateServiceConfig error = nil, want unsafe production platform config")
	}
	for _, token := range []string{"platform.session_secret", "platform.payment_secret"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("ValidateServiceConfig error = %v, want %s", err, token)
		}
	}
}

func TestValidateServiceConfigRejectsUnsafeProductionAdminGatewayConfig(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "admin_gateway")
	cfg.Set("sid", 7001)
	cfg.Set("env", "production")
	cfg.Set("admin_gateway.tokens", []map[string]any{
		{"token": "dev-admin-gateway-token", "operator_id": "local-admin", "role": "admin"},
	})
	cfg.Set("admin_gateway.targets", []map[string]any{
		{"environment": "production", "service_type": "game", "sid": 2001, "ops_addr": "127.0.0.1:9101", "dispatch_mode": "local_ops"},
	})

	err := ValidateServiceConfig(cfg)
	if err == nil {
		t.Fatal("ValidateServiceConfig error = nil, want unsafe production admin gateway config")
	}
	for _, token := range []string{"admin_gateway.tokens", "admin_gateway.default_ops_admin_token"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("ValidateServiceConfig error = %v, want %s", err, token)
		}
	}
}

// RR-20261005-NC-192（维护者决定 C1 方案 1）：生产校验只要求有读取方的设置。旧行为：game 必须写
// player.login_auth_required / player.login_secret / player_protocol.rate_limit.enabled / save_load.wal.*，
// instance 要 instance.state_store_required，account 要 account.ops_token / account.redis_required，
// global / match_group 要 <service>.redis_required——没有任何代码读取它们，写上只为让校验放行。
// 现在不写它们照样通过；写成 false 也不改变结果（它们不控制任何行为）。
func TestValidateServiceConfigDoesNotRequireSwitchesNothingReads(t *testing.T) {
	for _, serverType := range []string{"game", "instance", "account", "global", "match_group"} {
		cfg := productionServiceConfig(serverType)
		if err := ValidateServiceConfig(cfg); err != nil {
			t.Errorf("production %s without the unread switches: %v", serverType, err)
		}
		for _, key := range []string{
			"player.login_auth_required", "player_protocol.rate_limit.enabled", "save_load.wal.enabled",
			"save_load.wal.required", "instance.state_store_required", "account.redis_required",
			"global.redis_required", "match_group.redis_required",
		} {
			cfg.Set(key, false)
		}
		cfg.Set("save_load.wal.mode", "async")
		cfg.Set("instance.client_mode", "local")
		if err := ValidateServiceConfig(cfg); err != nil {
			t.Errorf("production %s with the unread switches off: %v", serverType, err)
		}
	}
}

func TestValidateServiceConfigRejectsProductionServicesWithoutRedis(t *testing.T) {
	for _, serverType := range []string{"game", "instance", "account", "global", "match_group"} {
		cfg := productionServiceConfig(serverType)
		cfg.Set("redis.addr", "")
		err := ValidateServiceConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "production "+serverType+" requires redis.addr") {
			t.Errorf("production %s without redis.addr: ValidateServiceConfig error = %v, want redis.addr", serverType, err)
		}
	}
}

// productionServiceConfig is a config that satisfies every OTHER production
// requirement, so a test can add exactly the one field it is about and be
// sure the rejection it sees is the one it asked for.
func productionServiceConfig(serverType string) *viper.Viper {
	cfg := viper.New()
	cfg.Set("server_type", serverType)
	cfg.Set("sid", 2001)
	cfg.Set("env", "production")
	cfg.Set("redis.addr", "127.0.0.1:6379")
	cfg.Set("account.session_secret", "session-secret")
	return cfg
}

// The helper is only useful if it is actually clean: a stale baseline would
// make every test built on it assert the wrong rejection.
func TestProductionServiceConfigBaselineIsValid(t *testing.T) {
	if err := ValidateServiceConfig(productionServiceConfig("game")); err != nil {
		t.Fatalf("production baseline is not valid: %v", err)
	}
}

// The ops endpoint exposes an unauthenticated /metrics and, with admin on,
// every registered admin command. Loopback is only the default, so production
// must not silently accept a public bind.
func TestValidateServiceConfigRejectsPublicOpsAddrInProduction(t *testing.T) {
	base := func() *viper.Viper {
		cfg := productionServiceConfig("game")
		cfg.Set("ops.enabled", true)
		return cfg
	}
	for name, addr := range map[string]string{
		"all interfaces":  "0.0.0.0:9100",
		"bare port":       ":9100",
		"specific public": "10.0.0.5:9100",
		"ipv6 wildcard":   "[::]:9100",
	} {
		cfg := base()
		cfg.Set("ops.addr", addr)
		err := ValidateServiceConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "ops.addr") {
			t.Fatalf("%s (%s): err=%v, want an ops.addr rejection", name, addr, err)
		}
	}
	for name, addr := range map[string]string{
		"ipv4 loopback": "127.0.0.1:9100",
		"ipv6 loopback": "[::1]:9100",
		"localhost":     "localhost:9100",
		"unset":         "",
	} {
		cfg := base()
		if addr != "" {
			cfg.Set("ops.addr", addr)
		}
		if err := ValidateServiceConfig(cfg); err != nil {
			t.Fatalf("%s (%s) rejected: %v", name, addr, err)
		}
	}
	// A declared opt-out is accepted: the operator is asserting there is an
	// authenticated proxy in front.
	cfg := base()
	cfg.Set("ops.addr", "0.0.0.0:9100")
	cfg.Set("ops.allow_public_addr", true)
	if err := ValidateServiceConfig(cfg); err != nil {
		t.Fatalf("declared opt-out rejected: %v", err)
	}
	// Outside production the bind address is the operator's business.
	dev := base()
	dev.Set("env", "dev")
	dev.Set("ops.addr", "0.0.0.0:9100")
	if err := ValidateServiceConfig(dev); err != nil {
		t.Fatalf("non-production public bind rejected: %v", err)
	}
	// A disabled ops endpoint cannot be exposed at all.
	off := base()
	off.Set("ops.enabled", false)
	off.Set("ops.addr", "0.0.0.0:9100")
	if err := ValidateServiceConfig(off); err != nil {
		t.Fatalf("disabled ops rejected: %v", err)
	}
}
