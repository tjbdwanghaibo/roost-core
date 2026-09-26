package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// RR-20260926-51（REPRO-2026-09-26-05 §4 改写）：RR-42 把“未声明 Mod × 5s 基准”与声明预算一起
// 按比例缩放，总时长足以覆盖声明值时也削减它（总 50s、dataengine 30s、10 个未声明 → 25s），声明值
// 偏大时又把先停的 Mod 压到接近零（总 30s、dataengine 120s → nest 1.07s，超时会中断整条关闭链）。
//
// 维护者批准的规则：先按声明值分配，上限为“剩余总时长 − 每个未声明 Mod 的固定保底”；只有声明值
// 之和超过上限才按比例缩放声明值；保底是固定下限、不参与缩放；总时长连所有 Mod 的保底都给不起时
// 均分并告警。保底为 3s（与生成配置 nest.request_timeout 同值：Nest 排空已准入请求的上限）。

// captureWarnings 把 slog 默认 logger 换成只收 Warn 及以上的文本 logger，测试结束恢复。
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func runStopBudgetPlan(t *testing.T, total time.Duration, mods []Mod) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	if err := stopModsReverseWithContext(ctx, mods, "test stop"); err != nil {
		t.Fatal(err)
	}
}

var auditStopOrder = []ModName{"etcd", "redis", "mongo", "nats", "lock", "remote_entity", "dataengine", "nest", "saga", "syncbus", "gateway"}

// REPRO §4 反例一：总 50s 足以覆盖 dataengine 声明的 30s 与其后 6 个未声明 Mod 的保底（6 × 3s），
// dataengine 必须拿到声明值，不能被“一起缩放”削减（修前 25s）。
func TestDeclaredStopBudgetIsGrantedWhenTotalCoversItAndTheFloors(t *testing.T) {
	logs := captureWarnings(t)
	rec := newBudgetRecorder()
	runStopBudgetPlan(t, 50*time.Second, stopBudgetMods(auditStopOrder, "dataengine", 30*time.Second, rec))
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), 30*time.Second)
	// 先停的 gateway 规划时声明值（30s）超过上限 50 − 10 × 3 = 20s，只拿固定保底。
	assertBudgetNear(t, "gateway", rec.get("gateway"), 3*time.Second)
	if strings.Contains(logs.String(), "dataengine") {
		t.Fatalf("dataengine got its declared budget but a scale-down warning was logged:\n%s", logs.String())
	}
}

// REPRO §4 反例二：dataengine.shutdown_timeout 调到 120s、总时长仍 30s。先停的 nest 必须保有固定
// 保底（修前按比例只剩 1.07s，会立即超时并中断整条关闭链）；dataengine 拿 30 − 3 × 3 = 21s 并告警。
func TestLargeDeclaredStopBudgetKeepsTheFloorOfEarlierMods(t *testing.T) {
	logs := captureWarnings(t)
	rec := newBudgetRecorder()
	runStopBudgetPlan(t, 30*time.Second, stopBudgetMods([]ModName{"redis", "mongo", "nats", "dataengine", "nest"}, "dataengine", 120*time.Second, rec))
	assertBudgetNear(t, "nest", rec.get("nest"), 3*time.Second)
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), 21*time.Second)
	// dataengine 立即结束后，其余 3 个未声明 Mod 均分剩下的约 30s。
	assertBudgetNear(t, "nats", rec.get("nats"), 10*time.Second)
	if !strings.Contains(logs.String(), "dataengine") || !strings.Contains(logs.String(), "scaled") {
		t.Fatalf("no scale-down warning for dataengine; logs:\n%s", logs.String())
	}
}

// 新的生成默认值：shutdown.total_timeout=60s、dataengine.shutdown_timeout=30s。REPRO §8 的三种 Mod
// 组合下 dataengine 都拿到声明值，先停的 Mod 均分 60 − 30 = 30s（5 / 7 / 10 个 Mod 时 7.5s / 5s / 3.33s），
// 每个未声明 Mod 都不低于 3s 保底，停机不告警。
func TestGeneratedDefaultShutdownBudgetsCoverEveryMod(t *testing.T) {
	logs := captureWarnings(t)
	const total, declared = 60 * time.Second, 30 * time.Second
	for _, order := range reproStopOrders {
		rec := newBudgetRecorder()
		runStopBudgetPlan(t, total, stopBudgetMods(order, "dataengine", declared, rec))
		label := fmt.Sprintf("%d mods: ", len(order))
		assertBudgetNear(t, label+"dataengine", rec.get("dataengine"), declared)
		first := order[len(order)-1]
		assertBudgetNear(t, label+string(first), rec.get(first), (total-declared)/time.Duration(len(order)-1))
		for _, name := range order {
			if name != "dataengine" && rec.get(name) < 3*time.Second-stopBudgetTolerance {
				t.Fatalf("%s%s budget=%v is below the 3s floor", label, name, rec.get(name).Round(time.Millisecond))
			}
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("the generated defaults logged a scale-down warning:\n%s", logs.String())
	}
}

// 总时长连每个 Mod 的保底都给不起（10s < 5 × 3s）：均分并告警，声明的 Mod 也不例外。
func TestStopBudgetsSplitEvenlyWhenTotalCannotCoverTheFloors(t *testing.T) {
	logs := captureWarnings(t)
	rec := newBudgetRecorder()
	runStopBudgetPlan(t, 10*time.Second, stopBudgetMods(reproStopOrders[0], "dataengine", 30*time.Second, rec))
	assertBudgetNear(t, "nest", rec.get("nest"), 2*time.Second)
	// nest 立即结束，剩约 10s、4 个 Mod，仍给不起保底：均分 2.5s。
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), 2500*time.Millisecond)
	if !strings.Contains(logs.String(), "floor") {
		t.Fatalf("no warning that the total cannot cover the per-mod floor; logs:\n%s", logs.String())
	}
}

// 没有任何 Mod 声明预算时与修前（“剩余 / 本段剩余 Mod 数”）等价，包括总时长低于保底之和的情形。
func TestUndeclaredStopBudgetsKeepTheEvenSplit(t *testing.T) {
	order := reproStopOrders[0]
	for _, total := range []time.Duration{30 * time.Second, 10 * time.Second} {
		rec := newBudgetRecorder()
		runStopBudgetPlan(t, total, stopBudgetMods(order, "", 0, rec))
		for i := len(order) - 1; i >= 0; i-- {
			// 前面的 Mod 立即结束，剩余时间几乎不变：第 k 个停止的 Mod 拿 total / 剩余数。
			assertBudgetNear(t, fmt.Sprintf("total %v: %s", total, order[i]), rec.get(order[i]), total/time.Duration(i+1))
		}
	}
}
