package roost

// RR-20260927-04（OPEN-ITEMS A08 + A09）：doctor 的 shutdown:<service> WARN 按 App 与 Mod 实际读配置的方式判定，
// 给出的建议值按配置里实际的声明预算算。
//
//  1. shutdown.total_timeout 为 0s 或负值时 App 兜底 30s（app.App.Execute）；FAIL 分支（configTotalTimeout）早已按 30s 算，
//     WARN 分支却按 0 算，说“0s 覆盖不了”并给建议值——对只需 18s 的框架服务是误报，对 game 服务说错了实际窗口。
//     dataengine.shutdown_timeout 为 0s 或负值时 kit/dataengine 同样兜底 30s，WARN 分支按 0 算会漏报。
//  2. “Set it to …” 的建议值用生成公式（dataengine 按常量 30s）算，配置把 dataengine.shutdown_timeout 调大后建议值偏小，
//     照着改完仍然 WARN。改为按该配置里的 dataengine.shutdown_timeout 算（声明值之和 + 3s × 未声明数 + 5s）。
//
// project sync 的生成公式不变（OPEN-ITEMS D11：仍按生成默认值 30s 计 dataengine）。

import (
	"strings"
	"testing"
	"time"
)

func gamePlan(t *testing.T, root string) serviceShutdown {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return serviceShutdownPlan(m, "game")
}

func syncProject(t *testing.T, root string) {
	t.Helper()
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
}

var gameConfigs = []string{
	"configs/service/config.game.yaml",
	"configs/service/config.game.prod.example.yaml",
	"deploy/k8s/base/secret.game.example.yaml",
}

var accountConfigs = []string{
	"configs/service/config.account.yaml",
	"configs/service/config.account.prod.example.yaml",
	"deploy/k8s/base/secret.account.example.yaml",
}

// A framework service needs 3s x 6 = 18s; a total_timeout of 0s or below runs
// on the App's 30s, which covers it.
func TestDoctorJudgesANonPositiveTotalAsTheAppsFallback(t *testing.T) {
	t.Parallel()
	for _, total := range []string{"0s", "-5s"} {
		t.Run(total, func(t *testing.T) {
			root := newGameDemo(t)
			for _, rel := range accountConfigs {
				setShutdownTotal(t, root, rel, total)
			}
			// The templates follow the App's 30s + 5s (RR-66 复核); without the
			// sync the FAIL for the old 28s grace period would come first.
			syncProject(t, root)
			item := shutdownStatus(t, root)["shutdown:account"]
			if item.Status != StatusOK {
				t.Fatalf("account with total_timeout %s (the App uses 30s, the Mods need 18s): %s %s, want OK", total, item.Status, item.Detail)
			}
			if !strings.Contains(item.Detail, "the App uses 30s") {
				t.Errorf("OK line does not say the App's fallback is what runs: %s", item.Detail)
			}
		})
	}
}

// The game service needs more than 30s: the WARN says so from the App's 30s,
// not from 0s.
func TestDoctorReportsTheAppsFallbackWhenItCannotCoverTheMods(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	dev := gameConfigs[0]
	setShutdownTotal(t, root, dev, "0s")
	syncProject(t, root)
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("game with total_timeout 0s: %s %s, want a WARN", item.Status, item.Detail)
	}
	if want := dev + ": total_timeout 0s (the App uses 30s) cannot cover"; !strings.Contains(item.Detail, want) {
		t.Errorf("WARN does not judge the App's 30s (%q): %s", want, item.Detail)
	}
}

// dataengine.shutdown_timeout 0s runs on kit/dataengine's 30s: a total that
// covers "0s declared" but not the real 30s is a WARN.
func TestDoctorJudgesANonPositiveDataEngineBudgetAsItsFallback(t *testing.T) {
	root := newGameDemo(t)
	dev := gameConfigs[0]
	plan := gamePlan(t, root)
	short := plan.total - generatedShutdownMargin - time.Second // one second short with dataengine at 30s
	setShutdownTotal(t, root, dev, seconds(short))
	setDataEngineShutdownTimeout(t, root, dev, "0s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, dev+": total_timeout "+seconds(short)+" cannot cover") {
		t.Fatalf("game with total_timeout %s and dataengine.shutdown_timeout 0s (kit/dataengine uses 30s): %s %s, want a WARN naming %s",
			seconds(short), item.Status, item.Detail, dev)
	}
}

// The advice is computed from the dataengine budget the configs set, and
// following it makes the WARN go away.
func TestDoctorAdviceFollowsTheConfiguredDataEngineBudget(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	for _, rel := range gameConfigs {
		setDataEngineShutdownTimeout(t, root, rel, "60s")
	}
	plan := gamePlan(t, root)
	want := plan.total + 30*time.Second // the formula's total with dataengine at 60s instead of 30s
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("dataengine.shutdown_timeout 60s with total_timeout %s: %s %s, want a WARN", seconds(plan.total), item.Status, item.Detail)
	}
	if advice := "Set it to " + seconds(want); !strings.Contains(item.Detail, advice) {
		t.Fatalf("WARN advice is not %q (60s dataengine budget): %s", advice, item.Detail)
	}
	for _, rel := range gameConfigs {
		setShutdownTotal(t, root, rel, seconds(want))
	}
	syncProject(t, root)
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK {
		t.Errorf("after following the advice (%s) and syncing: %s %s, want OK", seconds(want), item.Status, item.Detail)
	}
}

// Configs that set different dataengine budgets get the advice each needs.
func TestDoctorAdvicePerFileWhenTheConfigsDiffer(t *testing.T) {
	root := newGameDemo(t)
	prod, secret := gameConfigs[1], gameConfigs[2]
	plan := gamePlan(t, root)
	for _, rel := range []string{prod, secret} {
		setShutdownTotal(t, root, rel, "60s")
	}
	setDataEngineShutdownTimeout(t, root, secret, "45s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("%s %s, want a WARN", item.Status, item.Detail)
	}
	want := "Set it to " + seconds(plan.total) + " in " + prod + ", " + seconds(plan.total+15*time.Second) + " in " + secret
	if !strings.Contains(item.Detail, want) {
		t.Errorf("WARN advice is not %q: %s", want, item.Detail)
	}
}
