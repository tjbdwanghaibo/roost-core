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
	checkTaskModuleRows(t, report, map[string]int{"rules": 10, "enter": 29, "shop": 20, "stamina": 25, "wallet": 14, "bag": 17, "shop+rules": 2, "stamina+rules": 2})
	before := moduleProjectDigest(t, root)
	roost("module", "add", "stamina", "shop", "rules")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed project")
	}
	recipeMutationMustFail(t, root, "game/entities/player/rules.go", "Payload: slices.Clone(part.Payload)", "Payload: part.Payload", "./game/entities/player", "TestRulesReadersAreOrderedAndSnapshotsOwnTheirBytes", "ordered owned snapshot=")
	recipeMutationMustFail(t, root, "game/entities/player/rules.go", "part.MessageID == 0 || seen[part.MessageID]", "part.MessageID == 0 || false && seen[part.MessageID]", "./game/entities/player", "TestRulesDuplicateMessageIDRejectsTheWholeSnapshot", "duplicate rules accepted")
	recipeMutationMustFail(t, root, "game/entities/player/stamina_client_rules.go", "RegenMillis: rules.RegenMillis", "RegenMillis: rules.RegenMillis/1000", "./game/handler", "TestWithStaminaRulesExportsTheActualRecoveryRule", "stamina client rule=")
	recipeMutationMustFail(t, root, "game/entities/player/shop_client_rules.go", "if row.Off {", "if false && row.Off {", "./game/handler", "TestWithShopRulesExportsSortedVisibleGoods", "visible goods=")
	recipeMutationMustFail(t, root, "game/entities/player/shop_client_rules.go", "Price: row.Price", "Price: row.Price+1", "./game/handler", "TestWithShopRulesExportsSortedVisibleGoods", "shop rule=")
	recipeMutationMustFail(t, root, "game/entities/player/shop_goods.go", "return generated.ShopGoodsByID(goodsID)", "row,ok:=generated.ShopGoodsByID(goodsID);row.Price=10;return row,ok", "./game/handler", "TestWithShopRulesReadsNewTableAndKeepsOldSnapshot", "purchase diverged")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestRules|^TestShop|^TestWithShopRules|^TestWithStaminaRules", "./game/entities/player", "./game/handler", "./game/controllers/rules")
}
