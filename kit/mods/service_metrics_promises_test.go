package mods

// C6（docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md）：生成工程默认给每个服务装上
// servicemetrics.NewMetricsReporter，service_metrics.enabled: false 在配置里把它关掉；未设置或 true 保持
// collaborator 给的 Reporter；写成 off 这类非布尔值启动报错并点名键（与其他框架开关一样严格，A4 ① 起由声明检查）。

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/servicemetrics"
)

func TestServiceMetricsSwitch(t *testing.T) {
	supplied := servicemetrics.NewRecorder()
	apply := func(t *testing.T, value any) (servicemetrics.Reporter, error) {
		t.Helper()
		cfg := viper.New()
		if value != nil {
			cfg.Set("service_metrics.enabled", value)
		}
		var settings ServiceMetricsConfig
		if err := app.LoadConfig(cfg, &settings); err != nil {
			return nil, err
		}
		var reporter servicemetrics.Reporter = supplied
		settings.ApplyServiceMetrics(&reporter)
		return reporter, nil
	}
	for _, tc := range []struct {
		name  string
		value any
		on    bool
	}{{"unset", nil, true}, {"true", true, true}, {"false", false, false}, {"false-string", "false", false}} {
		t.Run(tc.name, func(t *testing.T) {
			reporter, err := apply(t, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if on := reporter != nil; on != tc.on {
				t.Fatalf("service_metrics.enabled=%v left the reporter on=%v, want %v", tc.value, on, tc.on)
			}
		})
	}
	if _, err := apply(t, "off"); err == nil || !strings.Contains(err.Error(), "service_metrics.enabled") {
		t.Fatalf("service_metrics.enabled: off: %v, want a refusal naming the key", err)
	}
}
