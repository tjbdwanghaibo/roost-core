package roost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleActorSelfSyncEnterThenCombat(t *testing.T) {
	root, roost, _ := moduleProject(t, "entercombat")
	roost("module", "add", "enter")
	verifyActorCombatSelf(t, root, []string{"enter"}, map[string]int{"enter": 14})
	roost("module", "add", "combat")
	verifyActorCombatSelf(t, root, []string{"actor", "combat", "enter"}, map[string]int{"actor": 12, "combat": 36, "enter": 14})
	checkActorCombatSelfPayload(t, root, "example.com/studio/entercombat")
}
func TestModuleActorSelfSyncCombatThenEnter(t *testing.T) {
	root, roost, _ := moduleProject(t, "combatenter")
	roost("module", "add", "combat")
	verifyActorCombatSelf(t, root, []string{"actor", "combat"}, map[string]int{"actor": 12, "combat": 36})
	roost("module", "add", "enter")
	verifyActorCombatSelf(t, root, []string{"actor", "combat", "enter"}, map[string]int{"actor": 12, "combat": 36, "enter": 14})
	checkActorCombatSelfPayload(t, root, "example.com/studio/combatenter")
}
func verifyActorCombatSelf(t *testing.T, root string, names []string, want map[string]int) {
	t.Helper()
	report, err := VerifyModules(context.Background(), root, names, nil)
	if err != nil {
		t.Fatalf("incremental self sync %v: %v %+v", names, err, report)
	}
	if len(report.Modules) != len(want) {
		t.Fatalf("unexpected rows %+v", report.Modules)
	}
	for _, r := range report.Modules {
		if !r.OK || r.Skipped != 0 || r.Passed != want[r.Name] {
			t.Fatalf("%+v", r)
		}
	}
}
func checkActorCombatSelfPayload(t *testing.T, root, module string) {
	t.Helper()
	marker, err := os.ReadFile(filepath.Join(root, "game/entities/player/entity.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(marker), "subjectPacker=NewActorPlayerSyncPacker") {
		t.Fatal("explicit Actor PlayerDao factory missing")
	}
	path := filepath.Join(root, "game/handler/actor_combat_self_generated_test.go")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(actorCombatSelfScenario, "MODULE", module)), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	runGeneratedGo(t, root, "test", "-count=1", "-run", "^TestGeneratedActorCombatSelfUsesOnlyPlayerDao$", "./game/handler")
}
func TestModuleActorSelfSyncRefusalRestoresEnterProject(t *testing.T) {
	root, roost, try := moduleProject(t, "combatpackerconflict")
	roost("module", "add", "enter")
	path := filepath.Join(root, "game/entities/player/entity.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "//roost:entity ", "//roost:entity subjectPacker=NewBusinessPacker ", 1))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	output, err := try("module", "add", "combat")
	if err == nil || !strings.Contains(output+err.Error(), "already has subjectPacker=") {
		t.Fatalf("business packer not refused: %v %s", err, output)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("refused combat left partial files or self sync changes")
	}
}

const actorCombatSelfScenario = `package handler
import (
 "errors"
 "fmt"
 "testing"
 "MODULE/db"
 player "MODULE/game/entities/player"
 "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
 "github.com/tjbdwanghaibo/roost-core/framework/entity"
 "github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
 corecombat "github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)
func TestGeneratedActorCombatSelfUsesOnlyPlayerDao(t *testing.T){
 f:=newActorFixture(t)
 f.must(func(p *player.Player)error{
  p.Dao().GetCombat().SetHealth(77)
  p.Dao().GetProfilePrivate().SetExp(913)
  p.Dao().GetCombatPrivate().SetSequence(123)
  p.Dao().GetCombatPrivate().SetRuntime([]byte("private checkpoint"))
  p.CombatComp().Core().InitCombatant(corecombat.Combatant{Alive:true,Health:99,MaxHealth:100})
  packer:=player.NewActorPlayerSyncPacker(p)
  snapshot,err:=packer.PackSubjectSnapshot(policy.SelfProfile);if err!=nil{return err}
  delta,err:=packer.PackSubjectDelta(policy.SelfProfile,dataengine.AllFields);if err!=nil{return err}
  for _,payload:=range []entity.FrozenSyncPayload{snapshot,delta}{
   copy:=db.NewPlayerDao();if err:=copy.ApplySync(payload.BytesCopy());err!=nil{return err}
   if copy.GetCombat().GetHealth()!=77||copy.GetProfilePrivate().GetExp()!=913{return fmt.Errorf("self projection missing or attached authority used")}
   if copy.GetCombatPrivate().GetSequence()!=0||len(copy.GetCombatPrivate().GetRuntimeAll())!=0{return fmt.Errorf("server checkpoint leaked")}
  }
  if _,err:=packer.PackSubjectSnapshot(entity.SyncProfile{Key:"scene"});!errors.Is(err,entity.ErrSyncViewUnknown){return fmt.Errorf("actor without scene published an unregistered view: %v",err)}
  return nil
 })
}
`
