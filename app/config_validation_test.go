package app

import (
	"testing"

	"github.com/spf13/viper"
)

func TestValidateServiceConfigAcceptsMinimalConfig(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 2001)

	if err := ValidateServiceConfig(cfg); err != nil {
		t.Fatalf("ValidateServiceConfig: %v", err)
	}
}

// productionServiceConfig 是满足 App 自己全部生产规则的配置：用例只加它要测的那一个键，看到的拒绝就是它要的那一条。
// 框架 Mod 的生产规则（密钥、ops 端点、Redis 地址）跟着声明走，用例在 kit（config_schema_promises_test.go）。
func productionServiceConfig(serverType string) *viper.Viper {
	cfg := viper.New()
	cfg.Set("server_type", serverType)
	cfg.Set("sid", 2001)
	cfg.Set("env", "production")
	return cfg
}

func TestProductionServiceConfigBaselineIsValid(t *testing.T) {
	if err := ValidateServiceConfig(productionServiceConfig("game")); err != nil {
		t.Fatalf("production baseline is not valid: %v", err)
	}
}
