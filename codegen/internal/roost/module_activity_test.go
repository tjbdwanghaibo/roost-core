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
	checkTaskModuleRows(t, report, map[string]int{"activity": 21, "activity_world": 4, "activity_alliance": 5})
	roost("module", "add", "profile", "leaderboard", "mailbox")
	report, err = VerifyModules(context.Background(), root, []string{"activity", "activity_world", "activity_alliance"}, nil)
	if err != nil {
		t.Fatalf("activity combined: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"activity": 21, "activity_world": 4, "activity_alliance": 5, "activity+enter": 2, "activity+profile": 1, "activity+leaderboard": 1, "activity+mailbox": 1})
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
	before := moduleProjectDigest(t, root)
	roost("module", "add", "activity_alliance", "activity_world", "activity")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("repeat install changed activity")
	}
	activityMutationChecks(t, root)
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestActivity|^TestWithActivity|^TestWorldActivity|^TestAllianceActivity", "./game/activity", "./game/handler", "./internal/rpc/activity_world", "./internal/rpc/activity_alliance")
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
	recipeMutationMustFail(t, root, "game/activity/engine.go", " || now >= r.GetCloseAt()", "", "./game/handler", "TestActivityFivePhasesAndDeadline", "deadline admission=")
	recipeMutationMustFail(t, root, "game/activity/engine.go", "if r.GetPending() {", "if false {", "./game/handler", "TestActivityAsyncInitializationAndSettlement", "initial=")
	recipeMutationMustFail(t, root, "game/activity/engine.go", "if h.token(token.Definition, r) != token {", "if false {", "./game/handler", "TestActivityCustomFlowAndLateEvents", "")
	recipeMutationMustFail(t, root, "game/activity/types.go", "AdvanceBudget  = 32", "AdvanceBudget  = 1000000", "./game/handler", "TestActivityLoopBoundedCatchupAndFrozenDefinition", "unbounded or missing catchup")
	recipeMutationMustFail(t, root, "game/activity/engine.go", "if record, ok := h.Book.GetRecords(d.ID); ok {", "if record, ok := h.Book.GetRecords(d.ID); ok && false {", "./game/handler", "TestActivityIndependentUnlockAndDuplicate", "anchors")
	recipeMutationMustFail(t, root, "game/activity/component.go", "if err := c.Host().Advance(fire.Now.UnixMilli()); err != nil {\n\t\tpanic(fmt.Errorf(\"activity timer: %w\", err))", "if err := c.Host().Advance(fire.Now.UnixMilli()); err != nil {\n\t\treturn 0", "./game/handler", "TestActivityTimerErrorKeepsDeadlineAndRollsBackHook", "timer swallowed")
	recipeMutationMustFail(t, root, "game/activity/rules.go", "next = midnight.Add(time.Duration(r.DayOffset) * time.Millisecond).UnixMilli()", "next = midnight.Add(time.Duration(r.DayOffset) * time.Millisecond + time.Hour).UnixMilli()", "./game/activity", "TestActivityBusinessCalendarDoesNotUse24Hours", "DST day=")
	recipeMutationMustFail(t, root, "game/activity/mailbox.go", "token.Generation, token.Cycle, action", "token.Generation, int64(1), action", "./game/activity", "TestWithActivityMailboxUsesStableInstanceKey", "")
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
