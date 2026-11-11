package roost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleTimerInstallAndFailureAreTransactional(t *testing.T) {
	t.Parallel()
	root, roost, try := moduleProject(t, "turnmodule")
	roost("add", "access", "player", "--service", "game")
	roost("add", "transport", "tcp")
	bad := filepath.Join(root, "game/handler/zz_broken.go")
	if err := os.WriteFile(bad, []byte("package handler\n//roost:nest rollback=undo durability=strict\nfunc handlerBroken()error{return nil}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if _, err := try("module", "add", "turn"); err == nil || !strings.Contains(err.Error(), "generate after installing modules") {
		t.Fatalf("expected generation failure: %v", err)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("failed turn install left DAO, timer, callback or bootstrap changes")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	before = moduleProjectDigest(t, root)
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "turn", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if moduleProjectDigest(t, root) != before || len(plan.Modules) != 2 || plan.Modules[1].Timer == nil || !plan.Modules[1].Timer.Create || plan.Modules[1].IDs["timer"]["TurnDeadline"] != 2 {
		t.Fatalf("plan: %+v", plan)
	}
	roost("module", "add", "turn")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Timers["room"].Types["TurnDeadline"] != 2 || m.Modules["turn"].TimerEntity != "room" || m.Modules["turn"].IDs["timer"]["TurnDeadline"] != 2 {
		t.Fatalf("timer ownership: %+v %+v", m.Timers, m.Modules["turn"])
	}
	raw, err := os.ReadFile(filepath.Join(root, "game/entities/room/timer_handlers.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "component.registerTurnTimers(handlers)") != 1 {
		t.Fatalf("callback missing or duplicated: %s", raw)
	}
	report, err := VerifyModules(context.Background(), root, []string{"room", "turn"}, nil)
	if err != nil || len(report.Modules) != 2 || report.Modules[0].Passed != 22 || report.Modules[1].Passed != 25 {
		t.Fatalf("verify: %v %+v", err, report)
	}
	before = moduleProjectDigest(t, root)
	roost("module", "add", "turn")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeated installation rewrote turn")
	}
}

func TestModuleTimerReusesCustomTimerAndAvoidsManualTypes(t *testing.T) {
	t.Parallel()
	root, roost, try := moduleProject(t, "turncustom")
	roost("module", "add", "room")
	roost("add", "timer", "--entity", "Room", "--component", "Deadline")
	file := filepath.Join(root, "game/entities/room/deadline_handlers.go")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\nconst (BusinessFirst int32 = 2+iota; BusinessSecond)\n")...)
	if err := os.WriteFile(file, raw, 0644); err != nil {
		t.Fatal(err)
	}
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "turn", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Modules[1].IDs["timer"]["TurnDeadline"] != 4 || plan.Modules[1].Timer.Create {
		t.Fatalf("custom allocation: %+v", plan)
	}
	// 接已有组件的登记也必须回滚，不能让未安装成功的模块挂上回调。
	bad := filepath.Join(root, "game/handler/zz_broken.go")
	if err := os.WriteFile(bad, []byte("package handler\n//roost:nest rollback=undo durability=strict\nfunc handlerBroken()error{return nil}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if _, err := try("module", "add", "turn"); err == nil {
		t.Fatal("invalid handler accepted")
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("failed install changed an existing timer")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	roost("module", "add", "turn")
	report, err := VerifyModules(context.Background(), root, []string{"room", "turn"}, nil)
	if err != nil || len(report.Modules) != 2 || report.Modules[0].Passed != 22 || report.Modules[1].Passed != 25 {
		t.Fatalf("custom timer verification: %v %+v", err, report)
	}
}

func TestModuleTimerConstantScanRejectsUnresolvedTypes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "timers.go")
	raw := `package room
import missing "example.com/unavailable"
const(A int32=1+iota;B;C)
const D int32=A+4
func unrelated(){missing.Anything()}
func register(h *missing.Handlers){h.Register(D,nil)}
`
	if err := os.WriteFile(file, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	used, names, err := scanModuleTimerConstants(dir)
	if err != nil || !used[1] || !used[2] || !used[3] || !used[5] || used[4] || !names["D"] {
		t.Fatalf("constants: %v %v %v", used, names, err)
	}
	raw = strings.Replace(raw, "h.Register(D,nil)", "h.Register(missing.DynamicType,nil)", 1)
	if err := os.WriteFile(file, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scanModuleTimerConstants(dir); err == nil || !strings.Contains(err.Error(), "cannot resolve Register timer type") {
		t.Fatalf("unknown type: %v", err)
	}
}

func TestModuleTimerAllocationIsPerEntityAndReservesRemovedTypes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := Manifest{Project: ProjectSpec{Module: "example.com/test"}, Timers: map[string]TimerSpec{
		"room":   {Component: "timer", ID: 8, Service: "game", Dao: "RoomDao", Types: map[string]int32{"Removed": 1}},
		"player": {Component: "timer", ID: 9, Service: "game", Dao: "PlayerDao"},
	}, Modules: map[string]InstalledModule{"old": {Service: "game", TimerEntity: "room", IDs: map[string]map[string]int64{"timer": {"Removed": 1}}}}}
	allocator := &moduleIDAllocator{manifest: m, used: map[string]map[int64]bool{}}
	plans := map[string]*ModuleTimerPlan{}
	first := map[string]map[string]int64{}
	if _, err := planModuleTimer(root, m, "game", ModuleSpec{Entity: "Room", TimerTypes: []string{"First"}}, false, allocator, plans, first); err != nil {
		t.Fatal(err)
	}
	second := map[string]map[string]int64{}
	if _, err := planModuleTimer(root, m, "game", ModuleSpec{Entity: "Room", TimerTypes: []string{"Second"}}, false, allocator, plans, second); err != nil {
		t.Fatal(err)
	}
	other := map[string]map[string]int64{}
	if _, err := planModuleTimer(root, m, "game", ModuleSpec{Entity: "Player", TimerTypes: []string{"First"}}, false, allocator, plans, other); err != nil {
		t.Fatal(err)
	}
	if first["timer"]["First"] != 2 || second["timer"]["Second"] != 3 || other["timer"]["First"] != 1 || len(m.Timers["room"].Types) != 1 {
		t.Fatalf("allocation mutated input or reused IDs: %v %v %v", first, second, other)
	}
	if err := m.validateModuleTimerRecords(); err != nil {
		t.Fatal(err)
	}
	m.Modules["old"].IDs["timer"]["Removed"] = 2
	if err := m.validateModuleTimerRecords(); err == nil {
		t.Fatal("changed timer ownership accepted")
	}
}

func TestModuleTimerRegistrationPreservesNamedReceiverAndIsIdempotent(t *testing.T) {
	t.Parallel()
	names := newTimerNames("example.com/test", "room", TimerSpec{Component: "deadline"})
	raw := []byte(`package room
func(t *DeadlineComponent)registerDeadlineHandlers(list *entitytimer.Handlers){
 // business registration stays here
 list.Register(Existing,t.existing)
}
`)
	once, err := appendModuleTimerRegistration(raw, "timers.go", names, "Turn")
	if err != nil {
		t.Fatal(err)
	}
	twice, err := appendModuleTimerRegistration(once, "timers.go", names, "Turn")
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) || !strings.Contains(string(once), "t.registerTurnTimers(list)") || !strings.Contains(string(once), "business registration stays here") {
		t.Fatalf("registration: %s", once)
	}
	conditional := strings.Replace(string(raw), "list.Register(Existing,t.existing)", "if disabled {return};list.Register(Existing,t.existing)", 1)
	if _, err := appendModuleTimerRegistration([]byte(conditional), "timers.go", names, "Turn"); err == nil {
		t.Fatal("registration hidden after conditional return")
	}
}

func TestModuleTimerRefusesOpaqueRegistration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "timers.go")
	raw := `package room
import entitytimer "github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
func register(h *entitytimer.Handlers){external.Register(h)}
`
	if err := os.WriteFile(file, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scanModuleTimerConstants(dir); err == nil || !strings.Contains(err.Error(), "escape static registration") {
		t.Fatalf("opaque registration: %v", err)
	}
}
