package roost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// demo只消费同一份Body移动机制；分区世界的DAO/协议/桥接继续由其MapComponent适配。
func TestDemoActorConsumesOneBodyAndPreservesMovementTransactions(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "game-demo")
	useLocalRoostCore(t, root)
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := renderModuleFile(manifest, "game", ModuleSpec{Name: "actor"}, "files", "game/actor/body.go", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "game/actor/body.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("demo Body differs from the actor module template")
	}
	if _, ok := manifest.Modules["actor"]; ok {
		t.Fatal("partial Body reuse installed an unused Actor component")
	}
	runGeneratedGo(t, root, "test", "-count=1", "-timeout", "90s", "-run", "^(TestPlayerSharedBody|TestAPlayerWalks|TestATwiceAsFast|TestARootedMonster|TestTheScenesBeat)", "./internal/service/game")
	mutations := []struct{ from, to, test, evidence string }{
		{"MaxPathPoints: 4096", "MaxPathPoints: 256", "TestPlayerSharedBodyRetainsDemoRouteBudget", "demo route budget shrank"},
		{"c.body.Stop()", "// injected: omit stopping the old walk", "TestAPlayerWalksToItsGoalOnItsOwnHeartbeatAndTheWatcherSeesTheSteps", "after a teleport"},
	}
	path := filepath.Join(root, "game/components/mapview/mapview.go")
	for _, m := range mutations {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(raw), m.from) != 1 {
			t.Fatalf("changed mutation anchor %q", m.from)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(raw), m.from, m.to, 1)), 0644); err != nil {
			t.Fatal(err)
		}
		output, compileErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-run", "^$", "./internal/service/game")
		if compileErr != nil {
			_ = os.WriteFile(path, raw, 0644)
			t.Fatalf("mutation did not compile: %v\n%s", compileErr, output)
		}
		output, runErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-count=1", "-timeout", "30s", "-v", "-run", "^"+m.test+"$", "./internal/service/game")
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
		if runErr == nil || !strings.Contains(string(output), "--- FAIL: "+m.test) || !strings.Contains(string(output), m.evidence) {
			t.Fatalf("mutation survived %s: %v\n%s", m.test, runErr, output)
		}
		t.Logf("compiled mutation caught: %s", m.test)
	}
}
