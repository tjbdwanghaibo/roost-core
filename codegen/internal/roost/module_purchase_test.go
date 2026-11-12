package roost

import (
	"context"
	"testing"
)

// 先装最小付费模块，再补背包与入场；场景只核对本次模块拥有的状态与回执。
func TestModulePurchaseScenariosAndMutations(t *testing.T) {
	root, roost, _ := moduleProject(t, "paidgame")
	roost("module", "add", "purchase")
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("purchase alone: %v %+v", err, report)
	}
	want := map[string]int{"purchase": 21, "wallet": 14}
	checkPurchaseRows(t, report, want)
	roost("module", "add", "bag", "enter")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("combined: %v %+v", err, report)
	}
	want = map[string]int{"purchase": 21, "wallet": 14, "bag": 17, "enter": 29, "purchase+bag": 4, "purchase+enter": 2}
	checkPurchaseRows(t, report, want)
	before := moduleProjectDigest(t, root)
	roost("module", "add", "enter", "bag", "purchase")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat installation changed project")
	}
	recipeMutationMustFail(t, root, "game/entities/player/purchase_component.go", "owner.WalletComp().Add(reward.Count)", "owner.WalletComp().Add(reward.Count+1)", "./game/handler", "TestPurchaseGrantCommitsRewardAndCursor", "facts seq=")
	recipeMutationMustFail(t, root, "game/entities/player/purchase_component.go", "if grant.Sequence <= applied {", "if false && grant.Sequence <= applied {", "./game/handler", "TestPurchaseReplayReturnsOriginalWithoutWriting", "replay=")
	recipeMutationMustFail(t, root, "game/entities/player/purchase_component.go", "if grant.Sequence != applied+1 {", "if false && grant.Sequence != applied+1 {", "./game/handler", "TestPurchaseOutOfOrderWaitsForPrevious", "sequence gap was accepted")
	recipeMutationMustFail(t, root, "game/entities/player/purchase_component.go", "if grant.PlayerID != c.owner.UniqueID() {", "if false && grant.PlayerID != c.owner.UniqueID() {", "./game/handler", "TestPurchaseOtherPlayersGrantIsRejected", "another player was accepted")
	recipeMutationMustFail(t, root, "game/entities/player/purchase_component.go", "if grant.Settled {", "if false && grant.Settled {", "./game/handler", "TestPurchaseSettledOrderOnlyAdvancesCursor", "facts seq=")
	recipeMutationMustFail(t, root, "game/entities/player/purchase_component.go", "if grant.Sequence <= applied {", "if grant.Sequence <= applied && applied < 512 {", "./game/handler", "TestPurchaseMoreThanAReceiptWindowKeepsOldReplaySafe", "old receipt=")
	recipeMutationMustFail(t, root, "game/purchase/queue.go", "product.AmountMinor != order.AmountMinor", "false", "./game/purchase", "TestPurchaseMismatchedMoneyDoesNotQueue", "calls=")
	recipeMutationMustFail(t, root, "game/controllers/purchase/controller.go", "if len(order.PendingAttempts) != 0 {", "if false && len(order.PendingAttempts) != 0 {", "./game/controllers/purchase", "TestPurchaseCollectionWaitsForInFlightDelivery", "in-flight delivery accepted")
	recipeMutationMustFail(t, root, "game/entities/player/purchase_item.go", "owner.BagComp().Add(reward.ID, reward.Count)", "owner.BagComp().Add(reward.ID, reward.Count+1)", "./game/handler", "TestWithPurchaseBagGrantAndReplay", "count=")
	recipeMutationMustFail(t, root, "internal/service/platform/purchase_channels.go", "return svcplatform.ErrSignatureInvalid", "_ = svcplatform.ErrSignatureInvalid; return nil", "./internal/service/platform", "TestPurchaseChannelDefaultsToDeny", "default channel admitted")
	recipeMutationMustFail(t, root, "game/controllers/purchase/controller.go", "default:\n\t\t\treturn receipts, svcplatform.ErrDeliveryHeld", "default:", "./game/controllers/purchase", "TestPurchaseCollectionWaitsForNonterminalOrder", "nonterminal order was granted")
	recipeMutationMustFail(t, root, "game/controllers/purchase/controller.go", "order.OrderID != grant.OrderID", "false", "./game/controllers/purchase", "TestPurchaseCollectionRejectsWrongOrder", "different order was granted")
	recipeMutationMustFail(t, root, "game/purchase/purchase.go", "grant.Digest != order.PayloadDigest", "false", "./game/purchase", "TestPurchaseExistingOrderRejectsChangedPayment", "changed payment err=")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestPurchase|^TestWithPurchase", "./game/handler", "./game/purchase", "./game/controllers/purchase", "./internal/service/platform")
}

func checkPurchaseRows(t *testing.T, report ModuleVerifyReport, want map[string]int) {
	t.Helper()
	if len(report.Modules) != len(want) {
		t.Fatalf("rows=%+v", report.Modules)
	}
	for _, row := range report.Modules {
		if row.Passed != want[row.Name] || row.Skipped != 0 || !row.OK {
			t.Fatalf("row=%+v", row)
		}
	}
}
