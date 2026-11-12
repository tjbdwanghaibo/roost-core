package roost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleMonsterInstallsWithRuntimeAndBehaviorMutations(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a monster project and verifies it")
	}
	root, roost, _ := moduleProject(t, "wildlife")
	roost("module", "add", "monster")
	before := moduleProjectDigest(t, root)
	roost("module", "add", "monster")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeated install changed source")
	}
	report, err := VerifyModules(context.Background(), root, []string{"monster"}, nil)
	if err != nil || len(report.Modules) != 1 || report.Modules[0].Passed != 26 || report.Modules[0].Skipped != 0 {
		t.Fatalf("verify: %v %+v", err, report)
	}
	mutations := []struct{ from, to, test string }{
		{"amount <= 0 || expectedLife != v.GetLifeSeq() ||", "amount <= 0 || false ||", "TestMonsterOldLifeDamageAndCommandsAreRejected"},
		{"if err := c.body.Leave(); err != nil {\n\t\treturn false, err\n\t}", "// injected: do not leave the scene on death", "TestMonsterDeathHidesButRetainsSpawnSlotUntilRespawn"},
		{"if b.GetTarget() == target && c.body.Move.Moving() {", "if false && b.GetTarget() == target && c.body.Move.Moving() {", "TestMonsterChasesPublishedPlayerWithoutRestartingSlowFollow"},
	}
	path := filepath.Join(root, "game/entities/monster/monster_actor_component.go")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range mutations {
		if strings.Count(string(original), mutation.from) != 1 {
			t.Fatalf("mutation anchor missing: %q", mutation.from)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(original), mutation.from, mutation.to, 1)), 0644); err != nil {
			t.Fatal(err)
		}
		output, compileErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-run", "^$", "./game/monster/service")
		if compileErr != nil {
			_ = os.WriteFile(path, original, 0644)
			t.Fatalf("mutation failed to compile:\n%s", output)
		}
		output, runErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-count=1", "-v", "-run", "^"+mutation.test+"$", "./game/monster/service")
		if err := os.WriteFile(path, original, 0644); err != nil {
			t.Fatal(err)
		}
		if runErr == nil || !strings.Contains(string(output), "--- FAIL: "+mutation.test) {
			t.Fatalf("mutation survived %s: %v\n%s", mutation.test, runErr, output)
		}
		t.Logf("compiled mutation killed by %s", mutation.test)
	}
}

func TestModuleMonsterDrainRecoveryMutations(t *testing.T) {
	root, roost, _ := moduleProject(t, "monsterdrainmutants")
	roost("module", "add", "monster")
	runGeneratedGo(t, root, "test", "-count=1", "-run", "^TestMonster(DrainRetriesWhenRemoveCommittedButReplyWasLost|DestroyRejectsAReplacementEntityInstance|.*InFlight)$", "./game/monster/service")
	mutants := []struct{ path, from, to, test, evidence string }{
		{"game/scene/service/runtime.go", "callbacks[kind] = callback", "if area.Dao().MembersLen() > 0 { callbacks[kind] = callback }", "TestMonsterDrainRetriesWhenRemoveCommittedButReplyWasLost", "drain retry leaked retired lifecycle record"},
		{"game/monster/service/runtime.go", "if current != r.value {", "if false && current != r.value {", "TestMonsterDestroyRejectsAReplacementEntityInstance", "known replacement reached Nest instead of early refusal"},
		{"game/handler/monster_spawn.go", "if err := expected.Check(target, area); err != nil {", "if err := expected.Check(target, area); false && err != nil {", "TestMonsterSpawnRejectsReplacementSceneInFlight", "was not rejected"},
		{"game/handler/monster_spawn.go", "if err := expected.Check(target, area); err != nil {", "if err := expected.Check(target, area); false && err != nil {", "TestMonsterRespawnRejectsReplacementMonsterInFlight", "was not rejected"},
		{"game/handler/monster_damage.go", "if err := expected.Check(target, area); err != nil {", "if err := expected.Check(target, area); false && err != nil {", "TestMonsterDamageRejectsReplacementMonsterInFlight", "was not rejected"},
		{"game/handler/monster_remove.go", "if err := expected.Check(target, area); err != nil {", "if err := expected.Check(target, area); false && err != nil {", "TestMonsterDestroyRejectsReplacementMonsterInFlight", "was not rejected"},
		{"game/entities/monster/target.go", "t.value != value", "false", "TestMonsterDamageRejectsReplacementMonsterInFlight", "was not rejected"},
		{"game/entities/monster/target.go", "t.area != area.Scene()", "false", "TestMonsterDamageRejectsReplacementSceneInFlight", "was not rejected"},
	}
	for _, m := range mutants {
		path := filepath.Join(root, m.path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(raw), m.from) != 1 {
			t.Fatalf("mutation anchor changed: %s %q", m.path, m.from)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(raw), m.from, m.to, 1)), 0644); err != nil {
			t.Fatal(err)
		}
		out, compileErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-run", "^$", "./game/monster/service")
		if compileErr != nil {
			_ = os.WriteFile(path, raw, 0644)
			t.Fatalf("mutation did not compile: %v %s", compileErr, out)
		}
		out, runErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-count=1", "-timeout", "30s", "-v", "-run", "^"+m.test+"$", "./game/monster/service")
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
		if runErr == nil || !strings.Contains(string(out), "--- FAIL: "+m.test) || !strings.Contains(string(out), m.evidence) {
			t.Fatalf("mutation survived: %v %s", runErr, out)
		}
		t.Logf("compiled mutation caught: %s", m.test)
	}
}
