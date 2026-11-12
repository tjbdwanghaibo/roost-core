package roost

import (
	"context"
	"testing"
)

// 新game上的真实双人Runtime、附加DAO与可选场景联动；断言必须能抓住可编译业务变异。
func TestModuleCombatScenariosAndMutations(t *testing.T) {
	root, roost, _ := moduleProject(t, "duelmodule")
	before := moduleProjectDigest(t, root)
	roost("module", "plan", "combat", "--json")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("combat plan wrote project")
	}
	roost("module", "add", "combat")
	verifyCombatModules(t, root, map[string]int{"combat": 38, "combat+skillbook": 1, "skillbook": 11}, "combat", "skillbook")
	roost("module", "add", "scene")
	verifyCombatModules(t, root, map[string]int{"combat": 38, "combat+skillbook": 1, "combat+scene": 2}, "combat")
	roost("module", "add", "equipment")
	verifyCombatModules(t, root, map[string]int{"combat": 38, "combat+skillbook": 1, "combat+scene": 2, "combat+equipment": 2}, "combat")

	recipeMutationMustFail(t, root, "game/entities/player/combat_component.go",
		"if len(raw) == 0 {", "if true {",
		"./game/handler", "TestCombatCooldownIsRestoredFromCheckpoint", "cooldown")
	recipeMutationMustFail(t, root, "game/combat/host.go",
		"if h.state.Hashes[p.ID()] != hash {", "if false && h.state.Hashes[p.ID()] != hash {",
		"./game/handler", "TestCombatExternalHPWriteRejectsWorldRestore", "HP boundary")
	recipeMutationMustFail(t, root, "game/combat/host.go",
		"StepBuffsToTick(int64(tick))", "StepBuffsToTick(int64(tick)-1)",
		"./game/handler", "TestCombatBuffExpiresThroughFormalDriver", "expired stun still gates")
	recipeMutationMustFail(t, root, "game/entities/player/combat_scene.go",
		`OnSceneFields("combat", "Combat")`, `_ = OnSceneFields`,
		"./game/controllers/scene", "TestWithCombatSceneSnapshotPublishesOnlyProjection", "combat public projection missing")
	roost("generate", "--check")
	before = moduleProjectDigest(t, root)
	roost("module", "add", "combat", "scene", "equipment")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("repeated combat installation changed project")
	}
}

func verifyCombatModules(t *testing.T, root string, want map[string]int, names ...string) {
	t.Helper()
	report, err := VerifyModules(context.Background(), root, names, nil)
	if err != nil {
		t.Fatalf("combat verify: %v %+v", err, report)
	}
	if len(report.Modules) != len(want) {
		t.Fatalf("combat rows %+v", report.Modules)
	}
	for _, result := range report.Modules {
		if result.Passed != want[result.Name] || result.Skipped != 0 || !result.OK {
			t.Fatalf("combat scenario count %+v", result)
		}
	}
}
