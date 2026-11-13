package roost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleSceneInstallsAndKeepsPublicViewsSeparate(t *testing.T) {
	root, roost, _ := moduleProject(t, "scenemodule")
	roost("module", "add", "scene")
	report, err := VerifyModules(context.Background(), root, []string{"actor", "scene"}, nil)
	if err != nil {
		t.Fatalf("verify scene: %v %+v", err, report)
	}
	want := map[string]int{"actor": 14, "scene": 28}
	for _, result := range report.Modules {
		if !result.OK || result.Skipped != 0 || result.Passed != want[result.Name] {
			t.Fatalf("%+v", result)
		}
	}
	marker, err := os.ReadFile(filepath.Join(root, "game/entities/player/entity.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(marker), "subjectPacker=NewActorPlayerSyncPacker") {
		t.Fatal("missing public view factory")
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "scene")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeated install changed business files")
	}
	recipeMutationMustFail(t, root, "game/entities/scene/entity.go", "s.Dao().MembersLen() != 0", "false && s.Dao().MembersLen() != 0", "./game/controllers/scene", "TestSceneOccupiedCloseIsRefused", "occupied map closed")
	recipeMutationMustFail(t, root, "game/handler/scene_interact.go", "sequence == state.GetSequence()", "false && sequence == state.GetSequence()", "./game/controllers/scene", "TestSceneInteractionIsOncePerContinuousSequence", "replayed interaction refused")
}

func TestModuleScenePlanRefusesExplicitBusinessPacker(t *testing.T) {
	root, roost, try := moduleProject(t, "sceneconflict")
	path := filepath.Join(root, "game/entities/player/entity.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "//roost:entity ", "//roost:entity sync=true subjectPacker=NewBusinessPacker ", 1))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	output, err := try("module", "plan", "scene")
	if err == nil || !strings.Contains(err.Error()+output, "already has subjectPacker=") {
		t.Fatalf("%v %s", err, output)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("refused plan wrote files")
	}
	_ = roost
}

func TestModuleSceneGluesUseExplicitPublicViewsAndNearbyChat(t *testing.T) {
	root, roost, _ := moduleProject(t, "sceneglues")
	roost("module", "add", "scene", "chat", "attribute", "equipment")
	report, err := VerifyModules(context.Background(), root, []string{"scene", "chat"}, nil)
	if err != nil {
		t.Fatalf("verify scene glues: %v %+v", err, report)
	}
	want := map[string]int{"scene": 28, "chat": 10, "scene+chat": 5, "chat+enter": 2, "scene+profile": 2, "scene+attribute": 2, "scene+equipment": 2}
	if len(report.Modules) != len(want) {
		t.Fatalf("unexpected glues %+v", report.Modules)
	}
	for _, result := range report.Modules {
		if !result.OK || result.Skipped != 0 || result.Passed != want[result.Name] {
			t.Fatalf("%+v", result)
		}
	}
	mutations := []struct{ path, from, to, test string }{
		{"game/components/scene/scene_profile.go", `OnSceneFields("profile", "Profile")`, `OnSceneFields("profile", "Profile", "ProfilePrivate")`, "TestWithSceneProfileSnapshotSeparatesPrivateFields"},
		{"game/components/actor/sync.go", "mask &= view.Fields", "mask &= view.Fields | dataengine.AllFields", "TestWithSceneProfileDeltaSeparatesPrivateFields"},
		{"game/entities/scene/entity.go", "s.Dao().GetClosed() || s.Dao().GetClosing()", "s.Dao().GetClosed()", "TestSceneStopRetriesRejectedEvacuation"},
		{"game/controllers/chat/scene_chat.go", "transport.ActiveSessions(recipient) > 0", "true", "TestWithSceneChatSkipsDisconnectedRecipients"},
	}
	for _, m := range mutations {
		path := filepath.Join(root, m.path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(raw), m.from) != 1 {
			t.Fatalf("mutation anchor %s: %q", m.path, m.from)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(raw), m.from, m.to, 1)), 0644); err != nil {
			t.Fatal(err)
		}
		output, compileErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-run", "^$", "./game/controllers/scene")
		if compileErr != nil {
			_ = os.WriteFile(path, raw, 0644)
			t.Fatalf("mutation does not compile: %v %s", compileErr, output)
		}
		output, runErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-count=1", "-v", "-run", "^"+m.test+"$", "./game/controllers/scene")
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
		if runErr == nil || !strings.Contains(string(output), "--- FAIL: "+m.test) {
			t.Fatalf("mutation survived %s: %v %s", m.path, runErr, output)
		}
		t.Logf("compiled mutation caught: %s", m.test)
	}
	roost("generate", "--check")
	before := moduleProjectDigest(t, root)
	roost("module", "add", "scene", "chat", "attribute", "equipment")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("repeat scene glue installation changed files")
	}
}

func TestModuleSceneReplacesOnlyTheGeneratedEnterSelfPacker(t *testing.T) {
	root, roost, _ := moduleProject(t, "sceneafterenter")
	roost("module", "add", "enter")
	roost("module", "add", "scene")
	report, err := VerifyModules(context.Background(), root, []string{"enter", "scene"}, nil)
	if err != nil {
		t.Fatalf("enter then scene: %v %+v", err, report)
	}
	for _, r := range report.Modules {
		if !r.OK || r.Skipped != 0 {
			t.Fatalf("%+v", r)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "game/entities/player/entity.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "subjectPacker=NewActorPlayerSyncPacker") {
		t.Fatal("generated self packer not replaced")
	}
}
