package roost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestModuleActivityGenerated(t *testing.T) {
	root, roost, _ := moduleProject(t, "activityplay")
	roost("module", "add", "activity_world", "activity_alliance")
	t.Logf("generated=%s", root)
	report, err := VerifyModules(context.Background(), root, []string{"activity", "activity_world", "activity_alliance"}, nil)
	if err != nil {
		t.Fatalf("activity: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"activity": 21, "activity_world": 6, "activity_alliance": 7})
	roost("module", "add", "profile", "leaderboard", "mailbox")
	report, err = VerifyModules(context.Background(), root, []string{"activity", "activity_world", "activity_alliance"}, nil)
	if err != nil {
		t.Fatalf("activity combined: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"activity": 21, "activity_world": 6, "activity_alliance": 7, "activity+enter": 2, "activity+profile": 1, "activity+leaderboard": 1, "activity+mailbox": 1})
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"activity_world", "alliance"} {
		if !contains(manifest.Services[name].Mods, "configdata") {
			t.Fatalf("%s has no rule loader", name)
		}
		raw, err := os.ReadFile(filepath.Join(root, moduleServiceConfig(name)))
		if err != nil || !bytes.Contains(raw, []byte("config_data:")) {
			t.Fatalf("%s config: %v %s", name, err, raw)
		}
	}
	// 恢复扫描是跑在属主服务里的 manager（D22）：登记进那个服务自己的列表，服务带上 manager Mod。
	for service, call := range map[string]string{"activity_world": "gamemanagers.NewWorldActivityScan()", "alliance": "gamemanagers.NewAllianceActivityScan()"} {
		raw, err := os.ReadFile(filepath.Join(root, "internal", "service", service, "managers.go"))
		if err != nil || !bytes.Contains(raw, []byte(call)) {
			t.Fatalf("%s managers.go does not register %s: %v\n%s", service, call, err, raw)
		}
		if !contains(manifest.Services[service].Mods, "manager") {
			t.Fatalf("%s has no manager mod: %v", service, manifest.Services[service].Mods)
		}
	}
	if got := manifest.Modules["activity_world"].ServiceManagers["activity_world"]; len(got) != 1 || got[0] != "NewWorldActivityScan" {
		t.Fatalf("roost.yaml does not record the service manager: %v", manifest.Modules["activity_world"].ServiceManagers)
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "activity_alliance", "activity_world", "activity")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("repeat install changed activity")
	}
	activityMutationChecks(t, root)
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestActivity|^TestWithActivity|^TestWorldActivity|^TestAllianceActivity", "./game/modules/activity", "./game/handler", "./game/managers", "./internal/rpc/activity_world", "./internal/rpc/activity_alliance")
}

func TestModuleExistingServiceExtensionKeepsOwnership(t *testing.T) {
	original := Manifest{Services: map[string]ServiceSpec{"game": {Mods: []string{"nest"}}, "alliance": {Mods: []string{"nest"}, Rpcs: []string{"alliance"}}}}
	spec := ModuleSpec{Name: "extension", Services: map[string]ModuleService{"alliance": {Existing: true, RPCs: []string{"activity_alliance"}}}}
	after, err := planModuleServices(t.TempDir(), original, "game", []ModuleSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(after.Services["alliance"].Rpcs, "activity_alliance") || contains(original.Services["alliance"].Rpcs, "activity_alliance") {
		t.Fatal("plan changed original ownership")
	}
	if _, err = planModuleServices(t.TempDir(), after, "game", []ModuleSpec{spec}); err == nil {
		t.Fatal("duplicate RPC allowed")
	}
	spec.Services["alliance"] = ModuleService{Existing: true, Entities: []string{"Alliance"}}
	if err = validateModuleServices(spec); err == nil {
		t.Fatal("extension took over entity ownership")
	}
}

func activityMutationChecks(t *testing.T, root string) {
	recipeMutationMustFail(t, root, "game/modules/activity/engine.go", " || now >= r.GetCloseAt()", "", "./game/handler", "TestActivityFivePhasesAndDeadline", "deadline admission=")
	recipeMutationMustFail(t, root, "game/modules/activity/engine.go", "if r.GetPending() {", "if false {", "./game/handler", "TestActivityAsyncInitializationAndSettlement", "initial=")
	recipeMutationMustFail(t, root, "game/modules/activity/engine.go", "if h.token(token.Definition, r) != token {", "if false {", "./game/handler", "TestActivityCustomFlowAndLateEvents", "")
	recipeMutationMustFail(t, root, "game/modules/activity/types.go", "AdvanceBudget  = 32", "AdvanceBudget  = 1000000", "./game/handler", "TestActivityLoopBoundedCatchupAndFrozenDefinition", "unbounded or missing catchup")
	recipeMutationMustFail(t, root, "game/modules/activity/engine.go", "if record, ok := h.Book.GetRecords(d.ID); ok {", "if record, ok := h.Book.GetRecords(d.ID); ok && false {", "./game/handler", "TestActivityIndependentUnlockAndDuplicate", "anchors")
	recipeMutationMustFail(t, root, "game/modules/activity/component.go", "if err := c.Host().Advance(fire.Now.UnixMilli()); err != nil {\n\t\tpanic(fmt.Errorf(\"activity timer: %w\", err))", "if err := c.Host().Advance(fire.Now.UnixMilli()); err != nil {\n\t\treturn 0", "./game/handler", "TestActivityTimerErrorKeepsDeadlineAndRollsBackHook", "timer swallowed")
	recipeMutationMustFail(t, root, "game/modules/activity/rules.go", "next = midnight.Add(time.Duration(r.DayOffset) * time.Millisecond).UnixMilli()", "next = midnight.Add(time.Duration(r.DayOffset) * time.Millisecond + time.Hour).UnixMilli()", "./game/modules/activity", "TestActivityBusinessCalendarDoesNotUse24Hours", "DST day=")
	recipeMutationMustFail(t, root, "game/modules/activity/mailbox.go", "token.Generation, token.Cycle, action", "token.Generation, int64(1), action", "./game/modules/activity", "TestWithActivityMailboxUsesStableInstanceKey", "")
	// 停 manager 时不收扫描：在途的推进没被取消、停机不等它。
	recipeMutationMustFail(t, root, "game/modules/activity/driver.go", "\treturn driver.Stop(ctx)\n}", "\treturn nil\n}", "./game/managers", "TestWorldActivityScanRunsUnderTheManagerAndStopsWithIt", "StopWithContext returned while an advance was still running")
	recipeMutationMustFail(t, root, "game/managers/alliance_activity_scan.go", "return m.scans.Close(ctx)", "return nil", "./game/managers", "TestAllianceActivityScanRunsUnderTheManagerAndStopsWithIt", "StopWithContext returned while an advance was still running")
	recipeMutationMustFail(t, root, "internal/rpc/activity_alliance/service.go", "return remoteentity.RPCRouteCheck(ctx, s.owners, s.sid, target)", "_ = remoteentity.RPCRouteCheck(ctx, s.owners, s.sid, target); return nil", "./internal/rpc/activity_alliance", "TestAllianceActivityChangedOwnerRejectsOldDriver", "route accepted foreign owner")
}

func TestModuleActivityMinimal(t *testing.T) {
	root, roost, _ := moduleProject(t, "activityminimal")
	roost("module", "add", "activity")
	report, err := VerifyModules(context.Background(), root, []string{"activity"}, nil)
	if err != nil {
		t.Fatalf("minimal activity: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"activity": 21})
}

func TestModuleActivityServiceRollback(t *testing.T) {
	root, roost, try := moduleProject(t, "activityrollback")
	roost("module", "add", "alliance", "activity")
	path := filepath.Join(root, "game/handler/zz_broken.go")
	if err := os.WriteFile(path, []byte("package handler\n//roost:nest rollback=undo durability=strict\nfunc handlerBroken()error{return nil}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if _, err := try("module", "add", "activity_alliance"); err == nil {
		t.Fatal("broken generation accepted")
	}
	if before != moduleProjectDigest(t, root) {
		t.Fatal("failed extension changed service, config or DAO")
	}
}
