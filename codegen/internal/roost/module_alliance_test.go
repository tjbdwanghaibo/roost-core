package roost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestModuleAllianceGenerated(t *testing.T) {
	root, roost, _ := moduleProject(t, "allianceplay")
	path := filepath.Join(root, ManifestName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reduced := regexp.MustCompile(`(?m)^\s*- remote_entity\n`).ReplaceAll(raw, nil)
	if bytes.Equal(raw, reduced) {
		t.Fatal("fixture lost remote_entity")
	}
	if err = os.WriteFile(path, reduced, 0644); err != nil {
		t.Fatal(err)
	}
	roost("project", "sync")
	roost("module", "add", "alliance")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(manifest.Services["alliance"].Rpcs, "alliance") || !contains(manifest.Services["game"].UsesRpcs, "alliance") {
		t.Fatal("alliance service and caller not installed")
	}
	config, err := os.ReadFile(filepath.Join(root, moduleServiceConfig("alliance")))
	if err != nil {
		t.Fatal(err)
	}
	// 开发配置不写 WAL 目录：运行期缺省 data/wal/<服务>/<sid> 让同 sid 的 game 与 alliance 各用各的目录
	// （wiring/dataengine 的 TestDataEngineModWALDirectoryIsPerServiceAndSid）。
	if bytes.Contains(config, []byte("dir: data/wal/")) {
		t.Fatal("the alliance dev config pins a WAL directory; the runtime default already separates services")
	}
	if len(manifest.Sagas) != 3 {
		t.Fatalf("sagas=%v", manifest.Sagas)
	}
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("alliance: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"bag": 20, "alliance": 55})
	before := moduleProjectDigest(t, root)
	roost("module", "add", "alliance", "--service", "game")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("repeat alliance installation changed source")
	}
	roost("module", "add", "team", "rehome", "enter")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("combined: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"alliance": 55, "bag": 20, "team": 35, "rehome": 44, "enter": 29, "alliance+enter": 2, "alliance+rehome": 2, "rehome+enter": 5, "team+rehome": 2})
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if contains(m.Services["game"].Mods, "remote_mirror") || !contains(m.Services["game"].Mods, "remote_entity") {
		t.Fatal("mirror promotion left duplicate mods")
	}
	recipeMutationMustFail(t, root, "game/handler/alliance_home.go", "seat.GetJoinRequestID() != joinRequestID || ", "", "./internal/rpc/alliance", "TestAllianceHomeCannotChangeRejoinedMembership", "stale=")
	recipeMutationMustFail(t, root, "game/handler/alliance_home.go", "current != expectedSID || ", "", "./internal/rpc/alliance", "TestAllianceHomeRejectsStaleSIDAndInvalidAddress", "stale=")
	recipeMutationMustFail(t, root, "game/handler/alliance_home.go", "seat.SetHomeSID(nextSID)", "seat.SetHomeSID(current)", "./internal/rpc/alliance", "TestAllianceHomeUpdatesOnce", "replay=")
	recipeMutationMustFail(t, root, "game/controllers/alliance/refresh.go", "!errors.Is(err, errcode.ErrEntityNotFound)", "!errors.Is(err, errcode.ErrEntityNotFound) && !errors.Is(err, context.DeadlineExceeded)", "./game/controllers/alliance", "TestAllianceRefreshKeepsDisplayOnUnknownFailure", "<nil>")
	recipeMutationMustFail(t, root, "game/controllers/alliance/refresh.go", "seat.JoinRequestID != reply.State.JoinRequestID || ", "", "./game/controllers/alliance", "TestAllianceRefreshRejectsStaleMirrorGeneration", "stale mirror")
	recipeMutationMustFail(t, root, "game/alliance/rehome.go", "return rehome.Binding(ctx, registry, playerID)", "_, _, _ = rehome.Binding(ctx, registry, playerID); return int32(1000), false, nil", "./game/alliance", "TestWithAllianceRehomeUsesCurrentBinding", "")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestAlliance|^TestWithAlliance", "./internal/rpc/alliance", "./game/handler", "./game/alliance/...", "./game/controllers/alliance")
}

// 本轮只跑联盟、换服接线与 demo 的相邻代码，不代替最终完整 demo 验收。
func TestDemoAllianceModuleIntegration(t *testing.T) {
	root := copyOfNewProject(t, "game-demo")
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "go.mod")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(raw, nil)
	raw = append(raw, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err = os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, root, "test", "-count=1", "-run", "^TestAlliance|^TestWithAlliance|^TestRehome|^TestTeam|^TestA.*Step|^TestThePlayerSteps", "./game/handler", "./game/alliance/...", "./game/controllers/alliance", "./internal/rpc/alliance", "./internal/service/game", "./db")
}

func TestModuleAllianceFailedGenerationRollsBackServiceAndSagas(t *testing.T) {
	root, roost, try := moduleProject(t, "alliancefail")
	roost("module", "add", "bag")
	roost("add", "mod", "saga", "-service", "game")
	roost("add", "transport", "tcp")
	broken := filepath.Join(root, "game", "handler", "zz_broken.go")
	if err := os.WriteFile(broken, []byte("package handler\n\n//roost:nest rollback=undo durability=strict\nfunc handlerBroken() error { return nil }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if out, err := try("module", "add", "alliance"); err == nil || !strings.Contains(err.Error(), "generate after installing modules") {
		t.Fatalf("want generation failure, got %v %s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("failed installation left service, receipts or saga files")
	}
}
