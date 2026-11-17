package roost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleVerifyPassed 把报告整理成 名字 -> (通过数, 跳过数)，失败时附上整份报告。
func moduleVerifyPassed(t *testing.T, report ModuleVerifyReport) map[string][2]int {
	t.Helper()
	got := map[string][2]int{}
	for _, result := range report.Modules {
		got[result.Name] = [2]int{result.Passed, result.Skipped}
	}
	return got
}

// 维护者 2026-11-17（PLAN-2026-11-17-r9-genre-modules §9.5）：模块场景在跑之前先按工程正式启动的方式装载工程的
// configs/data。卡牌 / 放置在 enter 的 EnterFirst 监听里发工程道具表里的道具；verify 跑 enter 自带的场景时如果
// 不装工程的表，bag 认不出这个道具号，enter 有 13 条答 bag_item_invalid。
func TestModuleVerifyLoadsTheProjectConfigData(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, _ := moduleProject(t, "ledger")
	roost("module", "add", "enter", "bag")

	items := filepath.Join(root, "configs", "table", "item.csv")
	rows, err := os.ReadFile(items)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(items, append(rows, []byte("2001,钻石,0\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "game/entities/player/gems.go", `package player

import "example.com/studio/ledger/game/event"

// OnGemsEnterFirst 在建档的那笔事务里发工程道具表里的道具（卡牌的钻石、放置的宝石都是这样发的）。
//
//roost:listen
func (e *Player) OnGemsEnterFirst(event.EnterFirst) error {
	_, err := e.Bag().Add(2001, 10)
	return err
}
`)
	roost("generate")

	report, err := VerifyModules(context.Background(), root, nil, nil)
	got := moduleVerifyPassed(t, report)
	if err != nil || got["enter"] != [2]int{29, 0} || got["bag"] != [2]int{20, 0} {
		t.Fatalf("verify = %v, err = %v; want enter 29 and bag 20 passing, 0 skipped\n%+v", got, err, report)
	}
}

// 维护者 2026-11-17（PLAN-2026-11-17-r9-genre-modules §9.5）：verify 只运行、只统计模块装进工程的那些测试文件里的用例。
// 工程自己的测试名字与模块场景同前缀（回合 PvP 的 TestTurnPVPRealTimerRecovery 之于 ^TestTurn）时不算进模块条数；
// 联动 <a>+<b> 的条数照同样的规则。
func TestModuleVerifyCountsOnlyTheModulesOwnTests(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, _ := moduleProject(t, "parlor")
	roost("module", "add", "enter", "profile")

	// 与 enter 的 ^TestEnter 同前缀：一条缺省档跳过的真实依赖用例、一条通过的工程用例。
	writeProjectFile(t, root, "game/handler/enter_project_test.go", `package handler

import (
	"os"
	"testing"
)

func TestEnterProjectRealDependencies(t *testing.T) {
	if os.Getenv("PARLOR_REAL") != "1" {
		t.Skip("set PARLOR_REAL=1")
	}
}

func TestEnterProjectOwnRule(t *testing.T) {}
`)
	// 与联动 enter+profile 的 ^TestWithEnterProfile 同前缀，放在联动场景所在的包里。
	writeProjectFile(t, root, "game/modules/login/enter_profile_project_test.go", `package login

import "testing"

func TestWithEnterProfileProjectOwn(t *testing.T) {
	t.Skip("the project's own test")
}
`)

	report, err := VerifyModules(context.Background(), root, nil, nil)
	got := moduleVerifyPassed(t, report)
	want := map[string][2]int{"enter": {29, 0}, "enter+profile": {3, 0}, "profile": {21, 0}, "profile+enter": {4, 0}}
	for name, counts := range want {
		if got[name] != counts {
			t.Errorf("%s = %v, want %v passing / skipped", name, got[name], counts)
		}
	}
	if err != nil || len(got) != len(want) || t.Failed() {
		var details []string
		for _, result := range report.Modules {
			if result.Detail != "" {
				details = append(details, result.Name+":\n"+result.Detail)
			}
		}
		t.Fatalf("verify = %v, err = %v\n%s", got, err, strings.Join(details, "\n"))
	}
}
