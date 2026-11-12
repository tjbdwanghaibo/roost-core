package roost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// actor 必须独立装在纯 game 上；plan 只展示，add 用正式 Mod 补齐心跳，重复安装不改业务文件。
func TestModuleActorInstallsHeartbeatAndRunsWithoutScene(t *testing.T) {
	root, roost, _ := moduleProject(t, "actorstandalone")
	before := moduleProjectDigest(t, root)
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "actor", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan)
	if !strings.Contains(string(raw), "roost add mod heartbeat -service game") || moduleProjectDigest(t, root) != before {
		t.Fatalf("heartbeat prerequisite or read-only plan: %s", raw)
	}
	roost("module", "add", "actor")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := manifest.Modules["scene"]; present {
		t.Fatal("actor installed scene")
	}
	resolved, err := resolveMods(effectiveServiceMods(manifest, "game"))
	if err != nil || !contains(resolved, "heartbeat") {
		t.Fatalf("mods %v: %v", resolved, err)
	}
	report, err := VerifyModules(context.Background(), root, []string{"actor"}, nil)
	if err != nil || len(report.Modules) != 1 || report.Modules[0].Passed != 14 || !report.Modules[0].OK {
		t.Fatalf("actor verification: %v %+v", err, report)
	}
	before = moduleProjectDigest(t, root)
	roost("module", "add", "actor")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeated installation changed project")
	}
}
