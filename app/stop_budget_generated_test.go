package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// RR-20260926-66（REPRO-2026-09-26-06 §2 改写）：生成配置对所有服务都写 shutdown.total_timeout: 60s，
// 而真实 game-demo 的 game 服务注册 23 个 Mod（20 个服务专属 + lock / ops / statslog 3 个共享），
// 只有 dataengine 声明预算（dataengine.shutdown_timeout: 30s）。60s 连 30s + 3s × 22 都给不起：
// 每次停机 WARN `cannot cover the per-mod floor`，先停的 accessplayertcp 只得约 3s 以下。
//
// 修复在生成器：按该服务实际注册的 Mod 计算 total_timeout = 声明预算之和 + 3s × 未声明 Mod 数 + 5s
// （余量给与 Mod 共用同一时限、先于 Mod 运行的 Service.Shutdown）。game 服务的生成值是 101s；
// codegen/internal/roost/shutdown_budget_test.go 从生成工程的 bootstrap 数出 23 个 Mod 并钉住同一个值。
const (
	generatedGameShutdownTotal  = 101 * time.Second
	generatedShutdownMargin     = 5 * time.Second
	generatedDataEngineDeclared = 30 * time.Second
)

// game-demo game 服务的 Mod（生成 bootstrap 的注册顺序）与共享 Mod。
var (
	generatedGameServiceMods = []ModName{"configdata", "mongo", "nats", "dataengine", "nest", "redis", "syncbus", "remote_entity", "saga",
		"c_account", "c_activity", "c_chat", "c_global", "c_mail", "c_match", "c_platform", "c_rank", "c_session", "accessplayer", "accessplayertcp"}
	generatedSharedMods = []ModName{"lock", "ops", "statslog"}
)

// withDataEngineAt 把 dataengine 挪到服务专属段的第 position 位（依赖排序可能改变它在停止顺序中的位置，
// 每个位置都要成立）。
func withDataEngineAt(position int) []ModName {
	out := make([]ModName, 0, len(generatedGameServiceMods))
	for _, name := range generatedGameServiceMods {
		if name != "dataengine" {
			out = append(out, name)
		}
	}
	out = append(out[:position], append([]ModName{"dataengine"}, out[position:]...)...)
	return out
}

func buildPlanMods(names []ModName) []Mod {
	mods := make([]Mod, len(names))
	for i, name := range names {
		plain := plainStopMod{name: name}
		if name == "dataengine" {
			mods[i] = declaringStopMod{plainStopMod: plain, declared: generatedDataEngineDeclared}
			continue
		}
		mods[i] = plain
	}
	return mods
}

// 模拟时钟：用 modStopBudget 按 App 的停止顺序逐个规划。Service.Shutdown 用掉 0 或整个余量；
// Mod 要么立即结束，要么 dataengine 之前的每个 Mod 用满给它的预算（最坏情况：dataengine 最晚拿到剩余）。
// 每个 Mod 都要拿到至少 3s 保底，dataengine 拿满声明的 30s，且全程不进入缩放 / 均分分支（不告警）。
func TestGeneratedGameServiceTotalCoversEveryModFloorAndTheDeclaredBudget(t *testing.T) {
	assertGameServiceBudgets(t, generatedGameShutdownTotal)
}

func assertGameServiceBudgets(t *testing.T, total time.Duration) {
	t.Helper()
	for position := range generatedGameServiceMods {
		for _, spent := range []time.Duration{0, generatedShutdownMargin} {
			for _, consume := range []string{"instant", "full-before-dataengine"} {
				name := fmt.Sprintf("dataengine@%d/service-shutdown=%v/%s", position, spent, consume)
				service, shared := buildPlanMods(withDataEngineAt(position)), buildPlanMods(generatedSharedMods)
				remaining := total - spent
				var problems []string
				for segment, mods := range [][]Mod{service, shared} {
					var later []Mod
					if segment == 0 {
						later = shared
					}
					reachedDataEngine := false
					for i := len(mods) - 1; i >= 0; i-- {
						budget, plan := modStopBudget(remaining, mods[i], mods[:i], later)
						mod := mods[i].Name()
						if plan != stopBudgetDeclaredGranted {
							problems = append(problems, fmt.Sprintf("%s plan=%d (warns)", mod, plan))
						}
						if mod == "dataengine" {
							reachedDataEngine = true
							if budget != generatedDataEngineDeclared {
								problems = append(problems, fmt.Sprintf("dataengine=%v, want %v", budget, generatedDataEngineDeclared))
							}
						} else if budget < undeclaredModStopFloor {
							problems = append(problems, fmt.Sprintf("%s=%v below the %v floor", mod, budget, undeclaredModStopFloor))
						}
						if consume == "full-before-dataengine" && segment == 0 && !reachedDataEngine {
							remaining -= budget
						}
					}
				}
				if len(problems) > 0 {
					t.Errorf("total=%v %s:\n  %s", total, name, strings.Join(problems, "\n  "))
				}
			}
		}
	}
}

// 真实时钟、正式停机入口（REPRO §2 第二个探针）：服务专属段 + 共享段，全部 Mod 立即结束。
func TestGeneratedGameServiceStopsWithoutBudgetWarnings(t *testing.T) {
	logs := captureWarnings(t)
	rec := newBudgetRecorder()
	service := stopBudgetMods(generatedGameServiceMods, "dataengine", generatedDataEngineDeclared, rec)
	shared := stopBudgetMods(generatedSharedMods, "", 0, rec)
	ctx, cancel := context.WithTimeout(context.Background(), generatedGameShutdownTotal)
	defer cancel()
	if err := stopModsReverseBefore(ctx, service, shared, "svc"); err != nil {
		t.Fatal(err)
	}
	if err := stopModsReverseWithContext(ctx, shared, "shared"); err != nil {
		t.Fatal(err)
	}
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), generatedDataEngineDeclared)
	for _, name := range append(append([]ModName{}, generatedGameServiceMods...), generatedSharedMods...) {
		if got := rec.get(name); got < undeclaredModStopFloor-250*time.Millisecond {
			t.Errorf("%s budget=%v, below the %v floor", name, got, undeclaredModStopFloor)
		}
	}
	if logs.Len() > 0 {
		t.Fatalf("stopping the generated game service logged warnings:\n%s", logs.String())
	}
}
