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
	want := map[string]int{"purchase": 23, "wallet": 17}
	checkPurchaseRows(t, report, want)
	roost("module", "add", "bag", "enter")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("combined: %v %+v", err, report)
	}
	want = map[string]int{"purchase": 23, "wallet": 17, "bag": 20, "enter": 29, "purchase+bag": 4, "purchase+enter": 2}
	checkPurchaseRows(t, report, want)
	before := moduleProjectDigest(t, root)
	roost("module", "add", "enter", "bag", "purchase")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat installation changed project")
	}
	recipeMutationMustFail(t, root, "game/components/purchase/purchase.go", "owner.Wallet().Add(reward.Count)", "owner.Wallet().Add(reward.Count+1)", "./game/handler", "TestPurchaseGrantCommitsRewardAndCursor", "facts seq=")
	recipeMutationMustFail(t, root, "game/components/purchase/purchase.go", "if grant.Sequence <= applied {", "if false && grant.Sequence <= applied {", "./game/handler", "TestPurchaseReplayReturnsOriginalWithoutWriting", "replay=")
	recipeMutationMustFail(t, root, "game/components/purchase/purchase.go", "if grant.Sequence != applied+1 {", "if false && grant.Sequence != applied+1 {", "./game/handler", "TestPurchaseOutOfOrderWaitsForPrevious", "sequence gap was accepted")
	recipeMutationMustFail(t, root, "game/components/purchase/purchase.go", "if grant.PlayerID != c.owner.UniqueID() {", "if false && grant.PlayerID != c.owner.UniqueID() {", "./game/handler", "TestPurchaseOtherPlayersGrantIsRejected", "another player was accepted")
	recipeMutationMustFail(t, root, "game/components/purchase/purchase.go", "if grant.Settled {", "if false && grant.Settled {", "./game/handler", "TestPurchaseSettledOrderOnlyAdvancesCursor", "facts seq=")
	recipeMutationMustFail(t, root, "game/components/purchase/purchase.go", "if grant.Sequence <= applied {", "if grant.Sequence <= applied && applied < 512 {", "./game/handler", "TestPurchaseMoreThanAReceiptWindowKeepsOldReplaySafe", "old receipt=")
	recipeMutationMustFail(t, root, "game/modules/purchase/queue.go", "product.AmountMinor != order.AmountMinor", "false", "./game/modules/purchase", "TestPurchaseMismatchedMoneyDoesNotQueue", "calls=")
	recipeMutationMustFail(t, root, "game/controllers/purchase.go", "if len(order.PendingAttempts) != 0 {", "if false && len(order.PendingAttempts) != 0 {", "./game/controllers", "TestPurchaseCollectionWaitsForInFlightDelivery", "in-flight delivery accepted")
	recipeMutationMustFail(t, root, "game/components/purchase/purchase_item.go", "owner.Bag().Add(reward.ID, reward.Count)", "owner.Bag().Add(reward.ID, reward.Count+1)", "./game/handler", "TestWithPurchaseBagGrantAndReplay", "count=")
	recipeMutationMustFail(t, root, "services/platform/purchase_channels.go", "return svcplatform.ErrSignatureInvalid", "_ = svcplatform.ErrSignatureInvalid; return nil", "./services/platform", "TestPurchaseChannelDefaultsToDeny", "default channel admitted")
	recipeMutationMustFail(t, root, "game/controllers/purchase.go", "default:\n\t\t\treturn receipts, granted, svcplatform.ErrDeliveryHeld", "default:", "./game/controllers", "TestPurchaseCollectionWaitsForNonterminalOrder", "nonterminal order was granted")
	recipeMutationMustFail(t, root, "game/controllers/purchase.go", "order.OrderID != grant.OrderID", "false", "./game/controllers", "TestPurchaseCollectionRejectsWrongOrder", "different order was granted")
	recipeMutationMustFail(t, root, "game/controllers/purchase.go", "if err := c.persist.WaitEntityProjection(ctx, full); err != nil {", "if err := error(nil); err != nil {", "./game/controllers", "TestPurchaseCollectionAcksOnlyAfterTheGrantsArePersisted", "orders acknowledged while the grant was only written, not persisted")
	recipeMutationMustFail(t, root, "game/modules/purchase/purchase.go", "grant.Digest != order.PayloadDigest", "false", "./game/modules/purchase", "TestPurchaseExistingOrderRejectsChangedPayment", "changed payment err=")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestPurchase|^TestWithPurchase", "./game/handler", "./game/modules/purchase", "./game/controllers", "./services/platform")
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
