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
	cfg.Set("account.session_secret", "dev-session-secret")
	cfg.Set("account.ops_token", "")

	err := ValidateServiceConfig(cfg)
	if err == nil {
		t.Fatal("ValidateServiceConfig error = nil, want unsafe production account config")
	}
	for _, token := range []string{"account.session_secret", "account.ops_token"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("ValidateServiceConfig error = %v, want %s", err, token)
		}
	}
}

func TestValidateServiceConfigRejectsProductionAccountWithoutRedisStore(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "account")
	cfg.Set("sid", 9201)
	cfg.Set("env", "production")
	cfg.Set("account.session_secret", "session-secret")
	cfg.Set("account.ops_token", "ops-token")
	cfg.Set("account.redis_required", false)
	cfg.Set("redis.addr", "")

	err := ValidateServiceConfig(cfg)
	if err == nil {
		t.Fatal("ValidateServiceConfig error = nil, want missing account redis store")
	}
	for _, token := range []string{"account.redis_required", "redis.addr"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("ValidateServiceConfig error = %v, want %s", err, token)
		}
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

func TestValidateServiceConfigRejectsProductionGameWithoutRuntimeGuards(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 2001)
	cfg.Set("env", "production")
	cfg.Set("redis.addr", "127.0.0.1:6379")
	cfg.Set("player.login_auth_required", true)
	cfg.Set("player.login_secret", "login-secret")
	cfg.Set("save_load.wal.enabled", true)
	cfg.Set("save_load.wal.required", true)
	cfg.Set("save_load.wal.mode", "async")
	cfg.Set("player_protocol.rate_limit.enabled", false)

	err := ValidateServiceConfig(cfg)
	if err == nil {
		t.Fatal("ValidateServiceConfig error = nil, want missing runtime guards")
	}
	for _, token := range []string{"player_protocol.rate_limit.enabled", "save_load.wal.mode"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("ValidateServiceConfig error = %v, want %s", err, token)
		}
	}
}

func TestValidateServiceConfigRejectsProductionGameWithoutRedis(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 2001)
	cfg.Set("env", "production")
	cfg.Set("player.login_auth_required", true)
	cfg.Set("player.login_secret", "login-secret")
	cfg.Set("player_protocol.rate_limit.enabled", true)
	cfg.Set("save_load.wal.enabled", true)
	cfg.Set("save_load.wal.required", true)
	cfg.Set("save_load.wal.mode", "durable")

	err := ValidateServiceConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "redis.addr") {
		t.Fatalf("ValidateServiceConfig error = %v, want redis.addr", err)
	}
}

func TestValidateServiceConfigRejectsProductionGameLocalInstanceClient(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 2001)
	cfg.Set("env", "production")
	cfg.Set("redis.addr", "127.0.0.1:6379")
	cfg.Set("player.login_auth_required", true)
	cfg.Set("player.login_secret", "login-secret")
	cfg.Set("player_protocol.rate_limit.enabled", true)
	cfg.Set("save_load.wal.enabled", true)
	cfg.Set("save_load.wal.required", true)
	cfg.Set("save_load.wal.mode", "durable")
	cfg.Set("instance.client_mode", "local")

	err := ValidateServiceConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "instance.client_mode") {
		t.Fatalf("ValidateServiceConfig error = %v, want instance.client_mode", err)
	}
}

func TestValidateServiceConfigRejectsProductionInstanceWithoutRuntimeGuards(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "instance")
	cfg.Set("sid", 8001)
	cfg.Set("env", "production")
	cfg.Set("redis.addr", "")
	cfg.Set("save_load.wal.enabled", true)
	cfg.Set("save_load.wal.required", false)
	cfg.Set("save_load.wal.mode", "async")
	cfg.Set("worldstage.state_store_required", false)
	cfg.Set("capitol.state_store_required", false)
	cfg.Set("warwarn.state_store_required", false)

	err := ValidateServiceConfig(cfg)
	if err == nil {
		t.Fatal("ValidateServiceConfig error = nil, want missing production instance guards")
	}
	for _, token := range []string{
		"redis.addr",
		"save_load.wal.required",
		"save_load.wal.mode",
		"instance.state_store_required",
	} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("ValidateServiceConfig error = %v, want %s", err, token)
		}
	}
}

func TestValidateServiceConfigRejectsProductionMatchGroupWithoutRedis(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "match_group")
	cfg.Set("sid", 9101)
	cfg.Set("env", "production")
	cfg.Set("match_group.redis_required", false)
	cfg.Set("redis.addr", "")

	err := ValidateServiceConfig(cfg)
	if err == nil {
		t.Fatal("ValidateServiceConfig error = nil, want missing match_group redis store")
	}
	for _, token := range []string{"match_group.redis_required", "redis.addr"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("ValidateServiceConfig error = %v, want %s", err, token)
		}
	}
}

func TestValidateServiceConfigRejectsProductionGlobalWithoutRedis(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "global")
	cfg.Set("sid", 9301)
	cfg.Set("env", "production")
	cfg.Set("global.redis_required", false)
	cfg.Set("redis.addr", "")

	err := ValidateServiceConfig(cfg)
	if err == nil {
		t.Fatal("ValidateServiceConfig error = nil, want missing global redis store")
	}
	for _, token := range []string{"global.redis_required", "redis.addr"} {
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("ValidateServiceConfig error = %v, want %s", err, token)
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
	cfg.Set("player.login_auth_required", true)
	cfg.Set("player.login_secret", "login-secret")
	cfg.Set("player_protocol.rate_limit.enabled", true)
	cfg.Set("save_load.wal.enabled", true)
	cfg.Set("save_load.wal.required", true)
	cfg.Set("save_load.wal.mode", "durable")
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
