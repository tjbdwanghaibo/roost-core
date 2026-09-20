package ops

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tjbdwanghaibo/roost-kit/mods"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/health"

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

	m.handleAdminCommands(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestOpsReadyReflectsLifecycleState(t *testing.T) {
	m := NewOpsMod()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	m.handleReady(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("initial ready status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	m.setReady(true, "ok")
	rec = httptest.NewRecorder()
	m.handleReady(rec, req)
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
	m.setReady(true, "ok")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	m.handleReady(rec, req)

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

func TestOpsModStopWithContextUsesCallerContext(t *testing.T) {
	mod := &OpsMod{server: &http.Server{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mod.StopWithContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StopWithContext err = %v, want context canceled", err)
	}
}

// The admin endpoint executes every registered admin command, so its token is
// a credential — compared in constant time, accepted from either header form,
// and never satisfied by an empty configured token.
func TestOpsAdminAuthorizationAcceptsOnlyTheExactToken(t *testing.T) {
	mod := &OpsMod{adminToken: "s3cret-token"}
	for name, header := range map[string]map[string]string{
		"dedicated header":    {"X-Admin-Token": "s3cret-token"},
		"bearer":              {"Authorization": "Bearer s3cret-token"},
		"lowercase bearer":    {"Authorization": "bearer s3cret-token"},
		"bearer with padding": {"Authorization": "  Bearer   s3cret-token  "},
		"bare authorization":  {"Authorization": "s3cret-token"},
	} {
		if !mod.authorized(requestWithHeaders(header)) {
			t.Fatalf("%s: valid token rejected", name)
		}
	}
	for name, header := range map[string]map[string]string{
		"no header":         {},
		"empty header":      {"X-Admin-Token": ""},
		"wrong token":       {"X-Admin-Token": "s3cret-tokeN"},
		"prefix of token":   {"X-Admin-Token": "s3cret"},
		"token plus suffix": {"X-Admin-Token": "s3cret-token-extra"},
		"wrong scheme":      {"Authorization": "Basic s3cret-token"},
		"bearer no token":   {"Authorization": "Bearer "},
	} {
		if mod.authorized(requestWithHeaders(header)) {
			t.Fatalf("%s: authorized when it must not be", name)
		}
	}
}

// Init refuses admin_enabled without a token, but an OpsMod assembled
// directly must not authorize a request that simply omits the header.
func TestOpsAdminAuthorizationRejectsEverythingWithoutAConfiguredToken(t *testing.T) {
	mod := &OpsMod{}
	for _, header := range []map[string]string{
		{},
		{"X-Admin-Token": ""},
		{"Authorization": "Bearer "},
		{"Authorization": ""},
	} {
		if mod.authorized(requestWithHeaders(header)) {
			t.Fatalf("empty configured token authorized %v", header)
		}
	}
}

func requestWithHeaders(headers map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/admin/execute", nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return request
}

type fakeStatsCollector struct{ record map[string]any }

func (c fakeStatsCollector) CollectStats() any { return c.record }

// /statsz serves the statslog observation as JSON when that Mod is assembled,
// and says so plainly when it is not — never an empty 200.
func TestOpsStatszServesTheStatsLogObservation(t *testing.T) {
	m := NewOpsMod()
	rec := httptest.NewRecorder()
	m.handleStats(rec, httptest.NewRequest(http.MethodGet, "/statsz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a registry status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	registry := app.NewRegistry(viper.New())
	m.registry = registry
	rec = httptest.NewRecorder()
	m.handleStats(rec, httptest.NewRequest(http.MethodGet, "/statsz", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("without statslog status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	if err := registry.Register(mods.ModStatsLog, fakeStatsCollector{record: map[string]any{"entity": map[string]any{"total": 3}}}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	m.handleStats(rec, httptest.NewRequest(http.MethodGet, "/statsz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":3`) {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}
