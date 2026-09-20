package mods

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// U-0150 · C2 · gap map kit `mods` 5/14：批量注册拒绝 nil 注册表、空能力名、批内重名；Redis 能力缺失时
// 报错并提示加 Mod；必填时长未设置时报错。
func TestRegisterAllAndLookupsRefuseMissingInputs(t *testing.T) {
	if err := RegisterAll(nil, Capability{Name: "x", Value: 1}); err == nil || !strings.Contains(err.Error(), "nil registry") {
		t.Fatalf("RegisterAll(nil) = %v", err)
	}
	registry := app.NewRegistry(viper.New())
	if err := RegisterAll(registry, Capability{Name: "", Value: 1}); err == nil || !strings.Contains(err.Error(), "capability name is empty") {
		t.Fatalf("RegisterAll with an unnamed capability = %v", err)
	}
	if err := RegisterAll(registry, Capability{Name: "x", Value: 1}, Capability{Name: "x", Value: 2}); err == nil || !strings.Contains(err.Error(), `duplicate capability "x"`) {
		t.Fatalf("RegisterAll with a duplicate = %v", err)
	}
	if _, ok := registry.Get("x"); ok {
		t.Fatal("a refused batch published a capability")
	}
	if _, err := Redis(registry); err == nil || !strings.Contains(err.Error(), "add roost-kit/redis.NewRedisMod()") {
		t.Fatalf("Redis without the redis mod = %v", err)
	}
	cfg := viper.New()
	if _, err := RequiredDuration(cfg, "mail.send_ttl"); err == nil || !strings.Contains(err.Error(), "is required and has no default") {
		t.Fatalf("RequiredDuration of an unset key = %v", err)
	}
	cfg.Set("mail.send_ttl", "5s")
	if d, err := RequiredDuration(cfg, "mail.send_ttl"); err != nil || d != 5*time.Second {
		t.Fatalf("RequiredDuration of a set key = (%v, %v)", d, err)
	}
}
