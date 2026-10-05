package configdata

// C2（维护者决定，2026-10-06）：新快照在 AfterApply 之前就已发布，这个语义保持；
// 但热更失败（整次拒绝）与回滚必须看得见。N07 第二批 C-O5：四次失败的 reload 在
// game 日志里 0 行、`configdata.reload.total` 没有失败计数——kit 只在监听者的
// AfterApply / Rollback 里计数，build / validate 阶段的失败到不了监听者。C-O6：
// 计数带运维自由填写的 `reason` 标签，每个不同的 reason 一条新序列。
// 承诺：每次 Load / Reload 恰好计一次 `configdata.reload.total{result=ok|failed}`，
// 发布后被撤回（AfterApply 失败）与运维 Rollback 计 `configdata.rollback.total{trigger}`，
// 标签只有这些低基数的取值；失败与回滚各留一条 Warn / Info 日志。

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	fconfigdata "github.com/tjbdwanghaibo/roost-core/configdata"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

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

	if got := counterValue(registryMetrics, "configdata.reload.total", metrics.Labels{"result": "ok"}); got != 2 {
		t.Errorf("configdata.reload.total{result=ok} = %d, want 2 (Start's load and the first reload)", got)
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
