package roost

import (
	"context"
	"testing"
)

func TestModuleTaskEventsPeriodsAndMutations(t *testing.T) {
	root, roost, _ := moduleProject(t, "questevents")
	roost("module", "add", "task")
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("minimal: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"task": 29, "wallet": 17, "task+wallet": 6})
	roost("module", "add", "checkin", "shop", "stamina", "dungeon", "leaderboard")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("combined: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{
		"task": 29, "wallet": 17, "checkin": 8, "shop": 20, "bag": 20, "stamina": 25, "profile": 20, "enter": 29, "profile+enter": 4, "enter+profile": 3, "dungeon": 21, "leaderboard": 21,
		"rules": 10, "shop+rules": 2, "stamina+rules": 2, "task+wallet": 6, "task+checkin": 3, "task+shop": 3, "task+stamina": 3, "task+profile": 2, "task+dungeon": 2, "task+bag": 3, "task+leaderboard": 3, "dungeon+leaderboard": 3,
	})
	before := moduleProjectDigest(t, root)
	roost("module", "add", "leaderboard", "dungeon", "stamina", "shop", "checkin", "task")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed files")
	}
	recipeMutationMustFail(t, root, "game/components/wallet/wallet.go", "return receipt.GetBalance(), nil", "return component.Add(delta)", "./game/handler", "TestWithTaskWalletOnlyCountsFreshSpend", "spend facts=")
	recipeMutationMustFail(t, root, "game/entities/player/task_wallet.go", "Report(\"wallet.spent\", 1, now)", "Report(\"wallet.spent\", 2, now)", "./game/handler", "TestWithTaskWalletOnlyCountsFreshSpend", "spend facts=")
	recipeMutationMustFail(t, root, "game/entities/player/task_shop.go", "Report(\"shop.bought\", count, now)", "Report(\"shop.bought\", 1, now)", "./game/handler", "TestWithTaskShopReportsBoughtQuantityOnce", "purchase=")
	recipeMutationMustFail(t, root, "game/entities/player/task_stamina.go", "Report(\"stamina.spent\", amount, now)", "Report(\"stamina.spent\", 1, now)", "./game/handler", "TestWithTaskStaminaCountsConsumedPoints", "wrong stamina progress")
	recipeMutationMustFail(t, root, "game/entities/player/task_profile.go", "int64(to-from)", "int64(to-from)+1", "./game/handler", "TestWithTaskProfileCountsLevelsNotCalls", "level=")
	recipeMutationMustFail(t, root, "game/entities/player/dungeon_component.go", "fctx.NowMilli()", "fctx.NowMilli()/1000", "./game/handler", "TestWithTaskDungeonReplayDoesNotCountAgain", "dungeon=")
	recipeMutationMustFail(t, root, "game/entities/player/task_component.go", "if day > state.GetDay() {", "if day != state.GetDay() {", "./game/handler", "TestTaskWeeklyRollbackDoesNotReopenClaim", "progress =")
	recipeMutationMustFail(t, root, "game/entities/player/task_component.go", "if definition.Previous == 0 {", "if true || definition.Previous == 0 {", "./game/handler", "TestTaskChainWaitsForClaimAndDoesNotReuseEvent", "locked=")
	recipeMutationMustFail(t, root, "game/entities/player/task_component.go", "progress = definition.Target", "progress += n", "./game/handler", "TestTaskAdvanceAccumulatesAndCapsAtTheTarget", "progress =")
	recipeMutationMustFail(t, root, "game/entities/player/task_component.go", "if claimed, _ := state.GetClaimed(taskID); claimed {", "if claimed, _ := state.GetClaimed(taskID); claimed && definition.Period != TaskPeriodPermanent {", "./game/handler", "TestTaskPermanentClaimSurvivesDaysAndRetirement", "retired replay=")
	recipeMutationMustFail(t, root, "game/entities/player/task_rewards.go", "owner.WalletComp().Add(reward.Count)", "owner.WalletComp().Add(reward.Count+1)", "./game/handler", "TestTaskRewardResponseNamesItsAsset", "reward=")
	recipeMutationMustFail(t, root, "game/entities/player/task_bag.go", "owner.BagComp().Add(reward.ID, reward.Count)", "owner.BagComp().Add(reward.ID, reward.Count+1)", "./game/handler", "TestWithTaskBagGrantsItemOnlyOnce", "item=")
	recipeMutationMustFail(t, root, "game/entities/player/task_catalog.go", "index.events[row.Event] = append(index.events[row.Event], row)", "index.events[\"win\"] = append(index.events[\"win\"], row)", "./game/handler", "TestTaskReportAdvancesOnlyMatchingEvents", "events=")
	recipeMutationMustFail(t, root, "game/components/wallet/wallet.go", "fctx.NowMilli()", "fctx.NowMilli()+1", "./game/handler", "TestWithTaskWalletUsesTheTransactionsFrozenDay", "frozen day=")
	recipeMutationMustFail(t, root, "game/components/wallet/wallet.go", "if err := spent.WalletSpent(-delta, fctx.NowMilli()); err != nil {", "if err := spent.WalletSpent(-delta, fctx.NowMilli()); false && err != nil {", "./game/handler", "TestWithTaskWalletHookFailureRollsBackBusinessAndProgress", "hook failure accepted")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestTask|^TestWithTask", "./game/handler", "./game/entities/player", "./game/controllers/task")
}
func checkTaskModuleRows(t *testing.T, report ModuleVerifyReport, want map[string]int) {
	t.Helper()
	if len(report.Modules) != len(want) {
		t.Fatalf("rows=%+v", report.Modules)
	}
	for _, row := range report.Modules {
		t.Logf("%s: pass=%d skip=%d ok=%v", row.Name, row.Passed, row.Skipped, row.OK)
		if row.Passed != want[row.Name] || row.Skipped != 0 || !row.OK {
			t.Fatalf("row=%+v", row)
		}
	}
}
