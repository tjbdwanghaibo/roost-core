package roost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 从零配置 game 安装，保持自定义 Go module；场景、构建、vet、ID、漂移均用正式验收入口。
func TestModuleNamingFriendsInstallVerifyAndMutations(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs module checks")
	}
	root, roost, _ := moduleProject(t, "namesocial")
	roost("module", "add", "naming", "friends")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(manifest.Services["game"].Mods, "directory") || !contains(manifest.Services["game"].Mods, "redis") {
		t.Fatalf("directory dependencies missing: %+v", manifest.Services["game"])
	}
	config, err := os.ReadFile(filepath.Join(root, "configs/service/config.game.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "key_prefix: roost:namesocial:directory") || !strings.Contains(string(config), "reservation_ttl: 60s") {
		t.Fatalf("directory starter config not isolated: %s", config)
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "friends", "naming")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed the project")
	}
	report, err := VerifyModules(context.Background(), root, []string{"naming", "friends"}, nil)
	if err != nil {
		t.Fatalf("verify: %v %+v", err, report)
	}
	counts := map[string]int{}
	for _, m := range report.Modules {
		counts[m.Name] = m.Passed
		if m.Skipped != 0 {
			t.Fatal("unexpected skip")
		}
	}
	if counts["naming"] != 15 || counts["friends"] != 18 {
		t.Fatalf("scenario counts: %v", counts)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("verify changed generated project")
	}
	// 先证明每个变异仍能编译，再要求对应业务断言失败；编译失败不算杀死变异。
	mutations := []struct{ path, from, to, pkg, test string }{
		{"game/entities/player/friends_component.go", "b.SetPeersKey(aid, true)", "// injected: omit the peer's friendship", "./game/handler", "TestFriendsAcceptWritesBothAndClearsCrossedApplications"},
		{"game/controllers/naming/naming_rename.go", "t, err = c.profile.Step(ctx, id, t.Seq, t.Operation, 2, t.ClaimKey, t.ClaimToken)\n\t\tif err != nil {\n\t\t\treturn \"\", err", "t, err = c.profile.Step(ctx, id, t.Seq, t.Operation, 2, t.ClaimKey, t.ClaimToken)\n\t\tif err != nil {\n\t\t\t_ = c.names.Release(ctx, claim.Key, claim.Owner)\n\t\t\treturn \"\", err", "./game/controllers/naming", "TestNamingProfileReplyLostNeverReleasesCurrentName"},
		{"game/handler/profile_rename.go", "}); ok {", "}); false && ok {", "./game/controllers/naming", "TestNamingOldSenderCannotBypassDirectory"},
		{"game/entities/player/naming_component.go", `fmt.Sprintf("naming:%d:%d", c.owner.ID(), s.GetSeq())`, `fmt.Sprintf("naming:%d", c.owner.ID())`, "./game/controllers/naming", "TestNamingLateCleanupCannotDeleteReusedName"},
	}
	for _, m := range mutations {
		path := filepath.Join(root, m.path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(raw), m.from) != 1 {
			t.Fatalf("mutation anchor %q in %s occurs %d times", m.from, m.path, strings.Count(string(raw), m.from))
		}
		changed := strings.Replace(string(raw), m.from, m.to, 1)
		if err := os.WriteFile(path, []byte(changed), 0644); err != nil {
			t.Fatal(err)
		}
		output, compileErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-run", "^$", m.pkg)
		if compileErr != nil {
			_ = os.WriteFile(path, raw, 0644)
			t.Fatalf("mutation did not compile: %s\n%s", m.path, output)
		}
		output, runErr := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-count=1", "-v", "-run", "^"+m.test+"$", m.pkg)
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
		if runErr == nil || !strings.Contains(string(output), "--- FAIL: "+m.test) {
			t.Fatalf("mutation survived %s: err=%v\n%s", m.path, runErr, output)
		}
		t.Logf("compiled mutation killed: %s by %s", m.path, m.test)
	}
}

// profile 独立安装仍经过正式旧 Sender，不能靠命名组合里的替身测试冒充兼容入口。
func TestModuleProfileWithoutNamingKeepsRenameProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a profile-only project")
	}
	root, roost, _ := moduleProject(t, "profilealone")
	roost("module", "add", "profile")
	output, err := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-race", "-count=1", "-v", "-run", "^TestProfileFormalRenameHonorsInstalledPolicy$", "./game/handler")
	if err != nil || strings.Count(string(output), "--- PASS: TestProfileFormalRenameHonorsInstalledPolicy") != 1 {
		t.Fatalf("profile standalone: %v\n%s", err, output)
	}
}
