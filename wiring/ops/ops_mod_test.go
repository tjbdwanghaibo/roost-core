package ops

import (
	"context"
	"encoding/json"

	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"

	"github.com/spf13/viper"
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
