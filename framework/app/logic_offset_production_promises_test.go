package app

import (
	"strings"
	"testing"
	"time"
)

// D-L3（维护者第六轮决定）：生产环境的业务时钟偏移必须为 0。time.logic_offset 是给测试环境
// 前拨业务时间用的；生产进程带着偏移启动，活动窗口、邮件过期、副本截止都会按错的时间走，
// 而且与没带偏移的进程错开。修前 ValidateServiceConfig 不看这个键，生产配置照样通过。
func TestProductionRefusesANonZeroLogicOffset(t *testing.T) {
	for _, offset := range []any{"24h", 24 * time.Hour, "-1s"} {
		cfg := productionServiceConfig("game")
		cfg.Set("time.logic_offset", offset)
		err := ValidateServiceConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "time.logic_offset") {
			t.Fatalf("production with time.logic_offset=%v: ValidateServiceConfig error = %v, want a refusal naming time.logic_offset", offset, err)
		}
	}
	// 0 与不写都放行；非生产环境任意偏移都放行。
	for _, offset := range []any{nil, "0s", time.Duration(0)} {
		cfg := productionServiceConfig("game")
		if offset != nil {
			cfg.Set("time.logic_offset", offset)
		}
		if err := ValidateServiceConfig(cfg); err != nil {
			t.Fatalf("production with time.logic_offset=%v: %v", offset, err)
		}
	}
	dev := productionServiceConfig("game")
	dev.Set("env", "dev")
	dev.Set("time.logic_offset", "24h")
	if err := ValidateServiceConfig(dev); err != nil {
		t.Fatalf("a dev config with an offset was refused: %v", err)
	}
}
