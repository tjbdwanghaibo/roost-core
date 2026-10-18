package configdata

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fconfigdata "github.com/tjbdwanghaibo/roost-core/framework/configdata"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func newProvidedConfigDataMod(t *testing.T, cfg *viper.Viper) (*Mod, *app.Registry) {
	t.Helper()
	mod := NewConfigDataMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	// app.NewRegistry pre-registers the platform capabilities (obs,
	// lifecycle) that Provide looks up.
	registry := app.NewRegistry(viper.New())
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	return mod, registry
}

func TestConfigDataStopStillReleasesHooksAfterDeadline(t *testing.T) {
	mod := &Mod{}
	released := false
	mod.unregisters = []func(){func() { released = true }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mod.StopWithContext(ctx); err != nil {
		t.Fatalf("fast local cleanup should complete after deadline: %v", err)
	}
	if !released || mod.unregisters != nil {
		t.Fatalf("cleanup not released: released=%v unregisters=%v", released, mod.unregisters)
	}
}

func TestConfigDataModInitHonorsConfiguredDir(t *testing.T) {
	cfg := viper.New()
	cfg.Set(cfgKeyDir, "custom/data")
	mod := NewConfigDataMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.dir != "custom/data" {
		t.Fatalf("dir=%q", mod.dir)
	}
	if mod.Store() == nil {
		t.Fatal("Init must create the store")
	}

	fallback := NewConfigDataMod()
	if err := fallback.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if fallback.dir != "configs/data" {
		t.Fatalf("default dir=%q", fallback.dir)
	}
}

func TestConfigDataModProvidesStoreAndStopUnregistersHooks(t *testing.T) {
	cfg := viper.New()
	cfg.Set(cfgKeyDir, t.TempDir()) // empty directory: a valid, empty dataset
	mod, registry := newProvidedConfigDataMod(t, cfg)

	store, ok := app.Lookup[*fconfigdata.Store](registry, mods.ModConfigData)
	if !ok || store == nil || store != mod.Store() {
		t.Fatal("store capability not registered")
	}
	if err := mod.Start(); err != nil {
		t.Fatalf("Start on an empty data dir: %v", err)
	}
	if len(mod.unregisters) == 0 {
		t.Fatal("Provide must register the metrics reload hook")
	}
	mod.Stop()
	if mod.unregisters != nil {
		t.Fatal("Stop must release the reload listeners")
	}
}

// Provide 对缺指标 / 生命周期能力的注册表拒绝；未 Init 就 Start 拒绝。
func TestConfigDataModRefusesBareRegistriesAndStartWithoutStore(t *testing.T) {
	if err := (&Mod{}).Provide(&app.Registry{}); err == nil || !strings.Contains(err.Error(), string(mods.ModMetrics)+`" not found`) {
		t.Fatalf("Provide with a bare registry = %v", err)
	}
	if err := (&Mod{}).Start(); err == nil || !strings.Contains(err.Error(), "store is nil") {
		t.Fatalf("Start without Init = %v", err)
	}
}

// 新快照在 AfterApply 之前就已发布（维护者决定 C2），但热更失败（整次拒绝）与回滚必须看得见：每次 Load / Reload 恰好计一次
// `configdata.reload.total{result=ok|unchanged|failed}`（build / validate 阶段的失败也算；unchanged 是发布成功但内容与上一代相同），发布后被撤回（AfterApply 失败）与运维 Rollback 计
// `configdata.rollback.total{trigger}`，标签只有这些低基数的取值（不带运维自由填写的 reason）；失败与回滚各留一条 Warn / Info 日志。

// captureLogs routes the default slog logger into a buffer for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func counterValue(registry *metrics.Registry, name string, labels metrics.Labels) int64 {
	var total int64
	for _, metric := range registry.Snapshot() {
		if metric.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if metric.Labels[k] != v {
				match = false
			}
		}
		if match {
			total += metric.Value
		}
	}
	return total
}

func TestFailedReloadAndRollbackAreCountedAndLogged(t *testing.T) {
	logs := captureLogs(t)
	dir := t.TempDir()
	cfg := viper.New()
	cfg.Set(cfgKeyDir, dir)
	mod, registry := newProvidedConfigDataMod(t, cfg)
	defer mod.Stop()
	registryMetrics := app.MustLookup[*metrics.Registry](registry, app.ModMetrics)
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}

	// A reload with a free-text reason succeeds: counted once, and the reason
	// does not become a label value.
	if _, err := mod.Store().ReloadWithReason(context.Background(), "s1-operator-typed-this"); err != nil {
		t.Fatal(err)
	}
	// A reload that fails before any listener runs (the data dir is gone).
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := mod.Store().ReloadWithReason(context.Background(), "after-delete"); err == nil {
		t.Fatal("reload of a missing data dir succeeded")
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A reload that is published and then taken back: a listener after the
	// metrics hook fails AfterApply.
	unregister := mod.Store().AddReloadListener(fconfigdata.ReloadHook{
		HookName:   "late-failure",
		AfterApply: func(context.Context, fconfigdata.ReloadEvent) error { return errors.New("late listener refuses") },
	})
	if _, err := mod.Store().ReloadWithReason(context.Background(), "late"); err == nil {
		t.Fatal("reload with a failing AfterApply succeeded")
	}
	unregister()

	// 数据目录为空，第一次重载与 Start 的加载内容相同：成功但计为 unchanged。
	if got := counterValue(registryMetrics, "configdata.reload.total", metrics.Labels{"result": "ok"}); got != 1 {
		t.Errorf("configdata.reload.total{result=ok} = %d, want 1 (Start's load)", got)
	}
	if got := counterValue(registryMetrics, "configdata.reload.total", metrics.Labels{"result": "unchanged"}); got != 1 {
		t.Errorf("configdata.reload.total{result=unchanged} = %d, want 1 (the first reload of the same empty dataset)", got)
	}
	if got := counterValue(registryMetrics, "configdata.reload.total", metrics.Labels{"result": "failed"}); got != 2 {
		t.Errorf("configdata.reload.total{result=failed} = %d, want 2 (missing dir, reverted publish)", got)
	}
	if got := counterValue(registryMetrics, "configdata.rollback.total", metrics.Labels{"trigger": "apply_failed"}); got != 1 {
		t.Errorf("configdata.rollback.total{trigger=apply_failed} = %d, want 1", got)
	}
	for _, metric := range registryMetrics.Snapshot() {
		if !strings.HasPrefix(metric.Name, "configdata.") {
			continue
		}
		for key := range metric.Labels {
			if key != "result" && key != "trigger" {
				t.Errorf("%s carries label %q=%q; only low-cardinality result / trigger are allowed", metric.Name, key, metric.Labels[key])
			}
		}
	}
	text := logs.String()
	for _, want := range []string{"config reload failed", "stage=build", "config reload reverted", "stage=apply"} {
		if !strings.Contains(text, want) {
			t.Errorf("log does not contain %q:\n%s", want, text)
		}
	}
}

func TestRevertedOperatorRollbackIsCounted(t *testing.T) {
	cfg := viper.New()
	cfg.Set(cfgKeyDir, t.TempDir())
	mod, registry := newProvidedConfigDataMod(t, cfg)
	defer mod.Stop()
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := mod.Store().ReloadWithReason(context.Background(), "new generation"); err != nil {
		t.Fatal(err)
	}
	live := mod.Store().Current()
	mod.Store().AddReloadListener(fconfigdata.ReloadHook{HookName: "refuse", AfterApply: func(context.Context, fconfigdata.ReloadEvent) error { return errors.New("late rollback refusal") }})
	if _, err := mod.Store().Rollback(context.Background(), "operator"); err == nil {
		t.Fatal("rollback unexpectedly succeeded")
	}
	reg := app.MustLookup[*metrics.Registry](registry, app.ModMetrics)
	if got := counterValue(reg, "configdata.rollback.total", metrics.Labels{"trigger": "apply_failed"}); got != 1 {
		t.Fatalf("reverted operator rollback count=%d, want 1", got)
	}
	if mod.Store().Current() != live {
		t.Fatal("refused rollback changed active generation")
	}
}

// 配置表重载是运维动作：Mod 在 Provide 时把 configdata 的 admin 命令挂到 App 的 admin 注册表（与 nats 挂
// bus.dlq.* 一样），业务不必再手写。内容没变的重载要明确说“没有变化”，而不是只报一个递增的版本号。
func TestConfigDataModServesReloadCommandsOnTheAdminRegistry(t *testing.T) {
	cfg := viper.New()
	cfg.Set(cfgKeyDir, t.TempDir())
	mod, registry := newProvidedConfigDataMod(t, cfg)
	defer mod.Stop()
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	commands := app.MustLookup[*admin.Registry](registry, app.ModAdmin)
	for _, name := range []string{fconfigdata.AdminCommandReload, fconfigdata.AdminCommandStatus, fconfigdata.AdminCommandRollback} {
		if !slices.Contains(commands.Names(), name) {
			t.Fatalf("admin registry lists %v, want %s registered by the configdata Mod", commands.Names(), name)
		}
	}
	status, err := commands.Execute(context.Background(), admin.Command{Name: fconfigdata.AdminCommandStatus})
	if err != nil || !status.OK || status.Data["version"] != uint64(1) {
		t.Fatalf("status = %+v err=%v, want version 1 after Start", status, err)
	}
	reload, err := commands.Execute(context.Background(), admin.Command{Name: fconfigdata.AdminCommandReload, Payload: []byte(`{"reason":"ops"}`)})
	if err != nil || !reload.OK {
		t.Fatalf("reload = %+v err=%v", reload, err)
	}
	if reload.Data["changed"] != false || !strings.Contains(reload.Message, "no table changed") {
		t.Fatalf("an unchanged reload answered %+v; want changed=false and a message saying no table changed", reload)
	}
}
