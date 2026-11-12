package modulehost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/infra/network/webroute"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"github.com/tjbdwanghaibo/roost-core/wiring/web"
)

const owner app.ModName = "service.account"

// recorder 记下模块启动与停止的先后。
type recorder struct{ events []string }

// module 造一个模块：启动与停止都记一笔；startErr / stopErr 让对应的一步失败；noStop 让它不返回停止函数。
func (r *recorder) module(name string, startErr, stopErr error, noStop bool) Module {
	return Module{Name: name, Start: func(registry *app.Registry) (func(context.Context) error, error) {
		if registry == nil {
			return nil, errors.New("no registry")
		}
		r.events = append(r.events, "start "+name)
		if noStop {
			return nil, startErr
		}
		return func(context.Context) error {
			r.events = append(r.events, "stop "+name)
			return stopErr
		}, startErr
	}}
}

func provided(t *testing.T, mod *Mod) *app.Registry {
	t.Helper()
	registry := app.NewRegistry(viper.New())
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

// 模块在 Start 阶段按给定顺序启动（Provide 时还没有启动），停机按反序；没有停止函数的模块不参与停机；
// 停过的不再停第二次。
func TestModulesStartInOrderAndStopInReverse(t *testing.T) {
	var r recorder
	mod := NewMod("account", owner, r.module("a", nil, nil, false), r.module("quiet", nil, nil, true), r.module("c", nil, nil, false))
	provided(t, mod)
	if len(r.events) != 0 {
		t.Fatalf("modules were started before the Mod's Start: %v", r.events)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.events, ", "); got != "start a, start quiet, start c" {
		t.Fatalf("start order = %s", got)
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.events[3:], ", "); got != "stop c, stop a" {
		t.Fatalf("stop order = %s, want the reverse of the start order", got)
	}
	mod.Stop()
	if err := mod.StopWithContext(context.Background()); err != nil || len(r.events) != 5 {
		t.Fatalf("stopping again: %v, events %v; a stopped module must not be stopped twice", err, r.events)
	}
	if err := mod.Start(); err == nil {
		t.Fatal("the modules were started a second time")
	}
}

// 启动到一半失败：错误点名模块与服务，后面的模块不启动；已经返回停止函数的（含失败的那个）由 Stop 反序收回。
func TestAFailedStartLeavesWhatStartedForStop(t *testing.T) {
	var r recorder
	broken := errors.New("port in use")
	mod := NewMod("account", owner, r.module("a", nil, nil, false), r.module("b", broken, nil, false), r.module("c", nil, nil, false))
	provided(t, mod)
	err := mod.Start()
	if !errors.Is(err, broken) || !strings.Contains(err.Error(), "module b") || !strings.Contains(err.Error(), "hosted service account") {
		t.Fatalf("Start = %v, want the module's error naming module b on hosted service account", err)
	}
	if got := strings.Join(r.events, ", "); got != "start a, start b" {
		t.Fatalf("after the failure: %s; module c must not start", got)
	}
	mod.Stop() // App 的启动失败收尾
	if got := strings.Join(r.events[2:], ", "); got != "stop b, stop a" {
		t.Fatalf("cleanup after the failed start = %s, want stop b, stop a", got)
	}
	// 失败而没有返回停止函数的模块：没有东西可停。
	var quiet recorder
	failing := NewMod("account", owner, quiet.module("a", nil, nil, false), quiet.module("b", broken, nil, true))
	provided(t, failing)
	if err := failing.Start(); !errors.Is(err, broken) {
		t.Fatal(err)
	}
	if err := failing.StopWithContext(context.Background()); err != nil || strings.Join(quiet.events, ", ") != "start a, start b, stop a" {
		t.Fatalf("cleanup = %v, %v", err, quiet.events)
	}
}

// 停止失败：停在那里并返回错误，它之前启动的模块保留（后启动的可能还在用它们）；再停一次从这里继续。
func TestAFailedStopKeepsTheEarlierModulesAndResumes(t *testing.T) {
	var r recorder
	stuck := errors.New("still draining")
	stopErr := stuck
	b := r.module("b", nil, nil, false)
	inner := b.Start
	b.Start = func(registry *app.Registry) (func(context.Context) error, error) {
		stop, err := inner(registry)
		return func(ctx context.Context) error {
			_ = stop(ctx)
			return stopErr
		}, err
	}
	mod := NewMod("account", owner, r.module("a", nil, nil, false), b, r.module("c", nil, nil, false))
	provided(t, mod)
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	err := mod.StopWithContext(context.Background())
	if !errors.Is(err, stuck) || !strings.Contains(err.Error(), "module b") {
		t.Fatalf("StopWithContext = %v, want module b's error", err)
	}
	if got := strings.Join(r.events[3:], ", "); got != "stop c, stop b" {
		t.Fatalf("after the failed stop: %s; module a must be kept", got)
	}
	stopErr = nil
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.events[3:], ", "); got != "stop c, stop b, stop b, stop a" {
		t.Fatalf("resuming the stop: %s", got)
	}
	// 停止函数拿到的是调用方的 ctx。
	var seen context.Context
	type key struct{}
	ctxMod := NewMod("account", owner, Module{Name: "ctx", Start: func(*app.Registry) (func(context.Context) error, error) {
		return func(ctx context.Context) error { seen = ctx; return nil }, nil
	}})
	provided(t, ctxMod)
	if err := ctxMod.Start(); err != nil {
		t.Fatal(err)
	}
	if err := ctxMod.StopWithContext(context.WithValue(context.Background(), key{}, "budget")); err != nil || seen == nil || seen.Value(key{}) != "budget" {
		t.Fatalf("the stop function did not receive the caller's context: %v", err)
	}
}

// 名字与依赖：modules.<服务>；硬依赖是托管服务的 owner Mod，web / redis / nats 在场时排在它前面。
// 构造参数不完整在 Init 报；没有 Provide 就 Start 报错。
func TestTheModDeclaresItsPlaceInTheProcess(t *testing.T) {
	mod := NewMod("account", owner)
	if mod.Name() != "modules.account" {
		t.Fatalf("name = %q", mod.Name())
	}
	if deps := mod.DependsOn(); !slices.Equal(deps, []app.ModName{owner}) {
		t.Fatalf("hard dependencies = %v, want the owner mod alone", deps)
	}
	for _, name := range []app.ModName{mods.ModWeb, mods.ModRedis, mods.ModNats} {
		if !slices.Contains(mod.OptionalDependsOn(), name) {
			t.Fatalf("optional dependencies %v do not order the %s mod first", mod.OptionalDependsOn(), name)
		}
	}
	if err := mod.Init(viper.New()); err != nil {
		t.Fatalf("a mod without modules is valid (nothing to start): %v", err)
	}
	if err := mod.Start(); err == nil {
		t.Fatal("Start before Provide succeeded")
	}
	if err := mod.Provide(nil); err == nil {
		t.Fatal("Provide accepted a nil registry")
	}
	start := func(*app.Registry) (func(context.Context) error, error) { return nil, nil }
	for label, broken := range map[string]*Mod{
		"no service":        NewMod("", owner),
		"no owner":          NewMod("account", ""),
		"a nameless module": NewMod("account", owner, Module{Start: start}),
		"no start function": NewMod("account", owner, Module{Name: "x"}),
		"a module twice":    NewMod("account", owner, Module{Name: "x", Start: start}, Module{Name: "x", Start: start}),
	} {
		if err := broken.Init(viper.New()); err == nil {
			t.Fatalf("%s: Init accepted it", label)
		}
	}
}

// 与 web Mod 一起走 App 的顺序：web Init → Provide → Start（bind），模块 Mod 在自己的 Start 里让模块登记路由，
// 之后 service.started 冻结路由表并开始 Serve——模块的路由可达。路由表冻结之后才启动的模块登记不进去，
// 错误原样带回；进程里没有 web Mod 时，模块自己的报错经本 Mod 带着模块名与服务名回来。
func TestAModuleRegistersItsRoutesBeforeTheRouteTableFreezes(t *testing.T) {
	cfg := viper.New()
	cfg.Set("web.addr", "127.0.0.1:0")
	registry := app.NewRegistry(cfg)
	webMod := web.NewMod()
	if err := webMod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := webMod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := webMod.Start(); err != nil {
		t.Fatal(err)
	}
	defer webMod.Stop()
	routes := func(path string) Module {
		return Module{Name: "routes" + path, Start: func(registry *app.Registry) (func(context.Context) error, error) {
			registerer, ok := app.Lookup[webroute.Registerer](registry, mods.ModWeb)
			if !ok {
				return nil, errors.New("capability web not found; roost add mod web -service account")
			}
			return nil, registerer.Register(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "from a module")
			})
		}}
	}
	early := NewMod("account", owner, routes("/early"))
	if err := early.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := early.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := early.Start(); err != nil {
		t.Fatalf("a module registering a route in the Mod's Start: %v", err)
	}
	life := app.MustLookup[*lifecycle.Registry](registry, mods.ModLifecycle)
	if err := life.Emit(context.Background(), lifecycle.Event{Phase: lifecycle.PhaseServiceStarted}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + webMod.Addr() + "/early")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "from a module" {
		t.Fatalf("GET /early = %d %q", response.StatusCode, body)
	}

	late := NewMod("account", owner, routes("/late"))
	_ = late.Init(cfg)
	_ = late.Provide(registry)
	if err := late.Start(); err == nil || !strings.Contains(err.Error(), "after the service started") || !strings.Contains(err.Error(), "module routes/late") {
		t.Fatalf("a module started after service.started: %v, want the web mod's refusal", err)
	}

	bare := NewMod("account", owner, routes("/nowhere"))
	provided(t, bare)
	if err := bare.Start(); err == nil || !strings.Contains(err.Error(), "roost add mod web -service account") || !strings.Contains(err.Error(), "hosted service account") {
		t.Fatalf("a module that needs the web mod in a process without it: %v", err)
	}
}
