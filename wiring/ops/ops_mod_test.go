package ops

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func TestOpsAdminRequiresExplicitSecureToken(t *testing.T) {
	cfg := viper.New()
	cfg.Set("ops.admin_enabled", true)
	cfg.Set("ops.admin_token", "dev-token")

	if err := NewOpsMod().Init(cfg); err == nil {
		t.Fatal("expected dev admin token to be rejected by default")
	}

	cfg.Set("ops.allow_dev_token", true)
	if err := NewOpsMod().Init(cfg); err != nil {
		t.Fatalf("Init with explicit dev allowance: %v", err)
	}
}

func TestOpsAdminEndpointIsHiddenWhenDisabled(t *testing.T) {
	m := NewOpsMod()
	cfg := viper.New()
	cfg.Set("ops.admin_enabled", false)
	if err := m.Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/commands", nil)
	rec := httptest.NewRecorder()

	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestOpsReadyReflectsLifecycleState(t *testing.T) {
	m := NewOpsMod()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("initial ready status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	m.SetReady(true, "ok")
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestOpsReadyIncludesDependencyHealth(t *testing.T) {
	reg := app.NewRegistry(viper.New())
	healthReg := app.MustLookup[*health.Registry](reg, app.ModHealth)
	healthReg.Register("redis", health.CheckerFunc(func(ctx context.Context) health.Result {
		return health.Result{Status: health.StatusFail, Message: "ping failed"}
	}))

	m := NewOpsMod()
	if err := m.Provide(reg); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	m.SetReady(true, "ok")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode ready: %v", err)
	}
	deps, ok := body["dependencies"].([]any)
	if !ok || len(deps) != 1 {
		t.Fatalf("dependencies = %+v", body["dependencies"])
	}
}

type fakeStatsCollector struct{ record map[string]any }

func (c fakeStatsCollector) CollectStats() any { return c.record }

// /statsz serves the statslog observation as JSON when that Mod is assembled,
// and says so plainly when it is not — never an empty 200.
func TestOpsStatszServesTheStatsLogObservation(t *testing.T) {
	m := NewOpsMod()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statsz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a registry status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	registry := app.NewRegistry(viper.New())
	if err := m.Provide(registry); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statsz", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("without statslog status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	if err := registry.Register(mods.ModStatsLog, fakeStatsCollector{record: map[string]any{"entity": map[string]any{"total": 3}}}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statsz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":3`) {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// 开了管理端口却没有 token 不能 Init；Provide 拒绝 nil 注册表与缺健康 / 指标 / 管理 / 生命周期能力的注册表
// （用空注册表一次覆盖第一条，其余三条与之同构且顺序在后）。
func TestOpsModRefusesTokenlessAdminAndBareRegistries(t *testing.T) {
	cfg := viper.New()
	cfg.Set("ops.admin_enabled", true)
	if err := NewOpsMod().Init(cfg); err == nil || !strings.Contains(err.Error(), "admin_enabled requires admin_token") {
		t.Fatalf("Init with admin enabled and no token = %v", err)
	}
	mod := NewOpsMod()
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(nil); err == nil || !strings.Contains(err.Error(), "app registry is nil") {
		t.Fatalf("Provide(nil) = %v", err)
	}
	// 四条能力查找是链式同文本：空注册表只能钉住第一条，所以核对它点名的是 health。
	if err := mod.Provide(&app.Registry{}); err == nil || !strings.Contains(err.Error(), string(mods.ModHealth)+`" not found`) {
		t.Fatalf("Provide with a bare registry = %v, want the health capability named", err)
	}
}

func TestOpsAdminTimeoutDefaultMatchesTheDeclaration(t *testing.T) {
	key, ok := NewOpsMod().ConfigSchema().Lookup("ops.admin_timeout")
	if !ok || key.Default != (10*time.Second).String() {
		t.Fatalf("ops.admin_timeout declared default = %q, want %s ((10*time.Second))", key.Default, (10 * time.Second))
	}
}

func TestProductionRejectsExplicitlyAllowedDevToken(t *testing.T) {
	cfg := viper.New()
	cfg.Set("env", "production")
	cfg.Set("ops.enabled", true)
	cfg.Set("ops.admin_enabled", true)
	cfg.Set("ops.admin_token", "dev-test")
	cfg.Set("ops.allow_dev_token", true)
	if err := NewOpsMod().Init(cfg); err == nil {
		t.Fatal("production accepted dev admin token")
	}
}
func TestAdminTokenIsDeclaredSecret(t *testing.T) {
	for _, key := range NewOpsMod().ConfigSchema().Keys {
		if key.Name == "ops.admin_token" {
			if !key.Secret {
				t.Fatal("admin token lacks the secret declaration")
			}
			return
		}
	}
	t.Fatal("missing token declaration")
}

func TestDisabledProductionAdminDoesNotRequireUnusedToken(t *testing.T) {
	cfg := viper.New()
	cfg.Set("env", "production")
	cfg.Set("ops.enabled", true)
	if err := NewOpsMod().Init(cfg); err != nil {
		t.Fatalf("disabled admin requires unused token: %v", err)
	}
}

// ops.enabled 时 Start 返回 nil 必须表示 /healthz、/readyz、/admin 已经在 ops.addr 上监听：端口被占用（另一个服务配了同一个
// ops 端口、旧进程还没退出）时 Start 必须失败，走正常的启动失败路径，而不是在没有探针端点的情况下继续运行。
func TestOpsStartFailsWhenTheAddressIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	m := newStartableOpsMod(t, taken.Addr().String())
	err = m.Start()
	if err == nil {
		_ = m.StopWithContext(context.Background())
		t.Fatalf("Start on %s (already in use) = nil, want the bind error: the process would run without its probe endpoints", taken.Addr())
	}
	if !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Fatalf("Start error = %v, want it to name ops.addr", err)
	}
	// 失败的 Start 不留下待关闭的 server：修正端口后可以再 Start（同一个 Mod 实例不会被 App 重用，
	// 这里只确认没有半截状态）。
	if err := m.StopWithContext(context.Background()); err != nil {
		t.Fatalf("Stop after a failed Start = %v", err)
	}
}

// newStartableOpsMod 走正式 Init / Provide，ops.enabled 打开、监听 addr。
func newStartableOpsMod(t *testing.T, addr string) *OpsMod {
	t.Helper()
	cfg := viper.New()
	cfg.Set("ops.enabled", true)
	cfg.Set("ops.addr", addr)
	m := NewOpsMod()
	if err := m.Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := m.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	return m
}
