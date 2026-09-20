//go:build integration

package nats

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// U-0153 · C2 · nats_mod.go:64 / 110（U-0150 留真机）：Provide 先连 NATS，连上之后缺健康注册表要拒绝；
// 开了可靠总线却没有 redis Mod 也要拒绝。`natsdriver.Assemble` 会真实连接，所以这两条只能对集成环境的
// NATS 验证。68（缺 admin 注册表）与 64 链式同文本、无法在有健康表而无管理表的注册表上单独触发，记冗余。
func TestRealNatsModProvideRefusesBareRegistriesAndReliableWithoutRedis(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run through scripts/integration/dataengine-env.sh")
	}
	ctx := context.Background()
	bare := viper.New()
	bare.Set("nats.url", url)
	mod := NewNatsMod(nil)
	if err := mod.Init(bare); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(ctx) })
	if err := mod.Provide(&app.Registry{}); err == nil || !strings.Contains(err.Error(), "health registry not found") {
		t.Fatalf("Provide with a bare registry = %v", err)
	}

	reliable := viper.New()
	reliable.Set("nats.url", url)
	reliable.Set("nats.reliable.enabled", true)
	reliable.Set("sid", 1)
	reliable.Set("server_type", "game")
	other := NewNatsMod(nil)
	if err := other.Init(reliable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.StopWithContext(ctx) })
	if err := other.Provide(app.NewRegistry(reliable)); err == nil || !strings.Contains(err.Error(), "nats reliable bus requires redis mod") {
		t.Fatalf("Provide with reliable enabled and no redis = %v", err)
	}
}
