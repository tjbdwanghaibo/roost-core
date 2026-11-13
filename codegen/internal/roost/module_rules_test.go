package roost

import (
	"context"
	"testing"
)

func TestModuleClientRulesAndShopTable(t *testing.T) {
	root, roost, _ := moduleProject(t, "clientrules")
	roost("module", "add", "rules")
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("rules: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"rules": 10, "enter": 29})
	roost("module", "add", "shop", "stamina")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("combined: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"rules": 10, "enter": 29, "shop": 20, "stamina": 25, "wallet": 17, "bag": 20, "shop+rules": 2, "stamina+rules": 2})
	before := moduleProjectDigest(t, root)
	roost("module", "add", "stamina", "shop", "rules")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed project")
	}
	recipeMutationMustFail(t, root, "game/components/rules/rules.go", "Payload: slices.Clone(part.Payload)", "Payload: part.Payload", "./game/components/rules", "TestRulesReadersAreOrderedAndSnapshotsOwnTheirBytes", "ordered owned snapshot=")
	recipeMutationMustFail(t, root, "game/components/rules/rules.go", "part.MessageID == 0 || seen[part.MessageID]", "part.MessageID == 0 || false && seen[part.MessageID]", "./game/components/rules", "TestRulesDuplicateMessageIDRejectsTheWholeSnapshot", "duplicate rules accepted")
	recipeMutationMustFail(t, root, "game/components/stamina/stamina_client_rules.go", "RegenMillis: rules.RegenMillis", "RegenMillis: rules.RegenMillis/1000", "./game/handler", "TestWithStaminaRulesExportsTheActualRecoveryRule", "stamina client rule=")
	recipeMutationMustFail(t, root, "game/components/shop/shop_client_rules.go", "if row.Off {", "if false && row.Off {", "./game/handler", "TestWithShopRulesExportsSortedVisibleGoods", "visible goods=")
	recipeMutationMustFail(t, root, "game/components/shop/shop_client_rules.go", "Price: row.Price", "Price: row.Price+1", "./game/handler", "TestWithShopRulesExportsSortedVisibleGoods", "shop rule=")
	recipeMutationMustFail(t, root, "game/components/shop/goods.go", "return generated.ShopGoodsByID(goodsID)", "row,ok:=generated.ShopGoodsByID(goodsID);row.Price=10;return row,ok", "./game/handler", "TestWithShopRulesReadsNewTableAndKeepsOldSnapshot", "purchase diverged")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestRules|^TestShop|^TestWithShopRules|^TestWithStaminaRules", "./game/components/rules", "./game/components/shop", "./game/handler", "./game/controllers/rules")
}
