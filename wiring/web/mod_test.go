package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/infra/network/webroute"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

type pingRoutes struct{}

func (pingRoutes) RegisterRoutes(reg webroute.Registerer) error {
	return reg.Register(http.MethodGet, "/ping", func(w http.ResponseWriter, r *http.Request) {
		webroute.WriteResult(w, http.StatusOK, map[string]string{"pong": "ok"}, nil)
	})
}

// 按 App 的顺序走一遍：Init → Provide → Start（只 bind）→ Service.Init 里 RegisterRoutes → service.started 开始
// Serve → 请求可达 → 之后再登记被拒 → service.stopping 停止监听。
func TestWebModServesRoutesRegisteredInServiceInit(t *testing.T) {
	cfg := viper.New()
	cfg.Set("web.addr", "127.0.0.1:0")
	registry := app.NewRegistry(cfg)
	mod := NewMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop()
	if err := RegisterRoutes(registry, pingRoutes{}); err != nil {
		t.Fatalf("RegisterRoutes in Service.Init: %v", err)
	}
	life := app.MustLookup[*lifecycle.Registry](registry, mods.ModLifecycle)
	if err := life.Emit(context.Background(), lifecycle.Event{Phase: lifecycle.PhaseServiceStarted}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + mod.Addr() + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var decoded map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &decoded) != nil || !strings.Contains(string(body), "pong") {
		t.Fatalf("GET /ping = %d %s", resp.StatusCode, body)
	}
	if err := mod.Register(http.MethodGet, "/late", func(http.ResponseWriter, *http.Request) {}); err == nil || !strings.Contains(err.Error(), "Service.Init") {
		t.Fatalf("Register after service.started = %v; want a refusal naming Service.Init", err)
	}
	if err := life.Emit(context.Background(), lifecycle.Event{Phase: lifecycle.PhaseServiceStopping}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get("http://" + mod.Addr() + "/ping"); err == nil {
		t.Fatal("GET /ping succeeded after service.stopping")
	}
}

// 端口被占用时 Start 失败（而不是进程没有路由地跑下去）；没装 web Mod 时 RegisterRoutes 报错并给命令。
func TestWebModStartFailsOnBusyPortAndRegisterNamesTheMod(t *testing.T) {
	first := NewMod()
	cfg := viper.New()
	cfg.Set("web.addr", "127.0.0.1:0")
	if err := first.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer first.Stop()
	busy := viper.New()
	busy.Set("web.addr", first.Addr())
	second := NewMod()
	if err := second.Init(busy); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(); err == nil || !strings.Contains(err.Error(), "web.addr") {
		t.Fatalf("Start on a busy port = %v; want an error naming web.addr", err)
	}
	if err := RegisterRoutes(app.NewRegistry(viper.New()), pingRoutes{}); err == nil || !strings.Contains(err.Error(), "roost add mod web") {
		t.Fatalf("RegisterRoutes without the web mod = %v", err)
	}
}

// 生产环境不默认把没有鉴权的业务路由暴露到公网。
func TestWebModRefusesPublicAddrInProductionUnlessAllowed(t *testing.T) {
	c := config{Enabled: true, Addr: "0.0.0.0:8080"}
	if err := c.ValidateConfig(true); err == nil || !strings.Contains(err.Error(), "web.allow_public_addr") {
		t.Fatalf("production public addr = %v", err)
	}
	c.AllowPublicAddr = true
	if err := c.ValidateConfig(true); err != nil {
		t.Fatal(err)
	}
	if err := (&config{Enabled: true, Addr: "127.0.0.1:8080"}).ValidateConfig(true); err != nil {
		t.Fatal(err)
	}
}
