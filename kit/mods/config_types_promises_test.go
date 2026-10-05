package mods

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// RR-20261005-NC-190：服务 Mod 读时长用的 Duration / RequiredDuration 不能把写错的值静默收下。
// 旧行为：`claim_ttl: 30`（不带单位）读成 30ns 并返回；`claim_ttl: abc` 读成 0，Duration 返回兜底值。
func TestServiceDurationsRefuseValuesWithoutAUnit(t *testing.T) {
	for _, text := range []string{"30", "\"30\"", "abc", "1.5"} {
		cfg := viper.New()
		cfg.SetConfigType("yaml")
		if err := cfg.ReadConfig(strings.NewReader("mail:\n  claim_ttl: " + text + "\n")); err != nil {
			t.Fatal(err)
		}
		if got, err := Duration(cfg, "mail.claim_ttl", time.Minute); err == nil || !strings.Contains(err.Error(), "mail.claim_ttl") {
			t.Fatalf("Duration(claim_ttl: %s) = %v, %v; want an error naming mail.claim_ttl", text, got, err)
		}
		if got, err := RequiredDuration(cfg, "mail.claim_ttl"); err == nil || !strings.Contains(err.Error(), "mail.claim_ttl") {
			t.Fatalf("RequiredDuration(claim_ttl: %s) = %v, %v; want an error naming mail.claim_ttl", text, got, err)
		}
	}
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader("mail:\n  claim_ttl: 30s\n  zero: 0\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := Duration(cfg, "mail.claim_ttl", time.Minute); err != nil || got != 30*time.Second {
		t.Fatalf("Duration(30s) = %v, %v", got, err)
	}
	if got, err := Duration(cfg, "mail.zero", time.Minute); err != nil || got != time.Minute {
		t.Fatalf("Duration(0) = %v, %v; want the fallback, as before", got, err)
	}
}
