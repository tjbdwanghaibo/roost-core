package roost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 新 game 不必预先开 attribute feature；先独立安装，再安装另一半，固定场景数不随组合改变。
func TestModuleAttributeEquipmentScenariosAndMutations(t *testing.T) {
	root, roost, _ := moduleProject(t, "attributeequipment")
	before := moduleProjectDigest(t, root)
	roost("module", "plan", "attribute", "--json")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("plan wrote project")
	}
	roost("module", "add", "attribute")
	verifyAttributeEquipment(t, root, []string{"attribute"}, map[string]int{"attribute": 20, "attribute+enter": 2})
	roost("module", "add", "equipment")
	verifyAttributeEquipment(t, root, []string{"attribute", "equipment"}, map[string]int{"attribute": 20, "equipment": 18, "attribute+equipment": 8, "attribute+enter": 2})
	// 真实默认 CSV 与工程原有升级钩子的顺序都在独立生成工程验证，不把 fixture 的 JSON 当默认数据。
	path := filepath.Join(root, "game/handler/attribute_equipment_generated_test.go")
	if err := os.WriteFile(path, []byte(attributeEquipmentGeneratedScenarios), 0600); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(root, "game/entities/player/profile_levelup.go")
	hook, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(attributeEquipmentOriginalHook), 0600); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, root, "test", "-count=1", "-v", "-run", "^TestGenerated(AttributeEquipmentDefaults|AttributeOriginalHookOrder)$", "./game/handler")
	if err := os.WriteFile(hookPath, hook, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	recipeMutationMustFail(t, root, "game/entities/player/profile_component.go",
		"if err := runProfileLevelUp(component, from, from+gained); err != nil {", "if err := error(nil); err != nil {",
		"./game/handler", "TestAttributeLevelUpGrowsBaseAndFinal", "level attributes")
	recipeMutationMustFail(t, root, "game/components/attribute/component.go",
		"coreattr.AttrValue(base.Attack + gear.Attack)", "coreattr.AttrValue(base.Attack)",
		"./game/handler", "TestWithAttributeEquipmentEquipRefreshesFinal", "gear final")
	recipeMutationMustFail(t, root, "game/entities/player/equipment_component.go",
		"c.owner.BagComp().Remove(itemID, 1)", "c.owner.BagComp().Remove(itemID, 2)",
		"./game/handler", "TestEquipmentEquipConsumesOne", "equip result")
	recipeMutationMustFail(t, root, "game/entities/player/equipment_component.go",
		"c.owner.BagComp().Add(previous, 1)", "c.owner.BagComp().Add(previous, 2)",
		"./game/handler", "TestEquipmentReplacementReturnsOldItem", "replacement")
	roost("generate", "--check")
	before = moduleProjectDigest(t, root)
	roost("module", "add", "attribute", "equipment")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("repeat install changed project")
	}
}

func TestModuleEquipmentInstallsIndependentlyAndAddsAttributeLater(t *testing.T) {
	root, roost, _ := moduleProject(t, "equipmentfirst")
	roost("module", "add", "equipment")
	verifyAttributeEquipment(t, root, []string{"equipment"}, map[string]int{"equipment": 18})
	roost("module", "add", "attribute")
	verifyAttributeEquipment(t, root, []string{"attribute", "equipment"}, map[string]int{"attribute": 20, "equipment": 18, "attribute+equipment": 8, "attribute+enter": 2})
}

func verifyAttributeEquipment(t *testing.T, root string, names []string, want map[string]int) {
	t.Helper()
	report, err := VerifyModules(context.Background(), root, names, nil)
	if err != nil {
		t.Fatalf("verify %v: %v %+v", names, err, report)
	}
	if len(report.Modules) != len(want) {
		t.Fatalf("scenario rows %+v", report.Modules)
	}
	for _, r := range report.Modules {
		if r.Passed != want[r.Name] || r.Skipped != 0 || !r.OK {
			t.Fatalf("scenario count %+v", r)
		}
	}
}

const attributeEquipmentOriginalHook = `package player
import apperrors "example.com/studio/attributeequipment/internal/errors"
func(c *ProfileComponent)onLevelUp(from,to int32)error{
 c.Owner().Dao().GetProfile().SetName("original")
 if from==1{return apperrors.ErrProfileNameEmpty}
 return nil
}
`
const attributeEquipmentGeneratedScenarios = `package handler
import(
 "context"
 "errors"
 "reflect"
 "sync/atomic"
 "testing"
 generated "example.com/studio/attributeequipment/configs/generated"
 player "example.com/studio/attributeequipment/game/entities/player"
 "github.com/tjbdwanghaibo/roost-core/framework/configdata"
)
var originalHookCalls atomic.Int64
func init(){player.OnProfileLevelUp("aaa_original_test",func(c *player.ProfileComponent,from,to int32)error{
 if c.DisplayName()!="original"{return errors.New("named hook ran before original")}
 originalHookCalls.Add(1);return nil
})}
func TestGeneratedAttributeEquipmentDefaults(t *testing.T){
 f:=newEquipmentFixture(t)
 reg:=configdata.NewRegistry();if err:=generated.RegisterGeneratedConfigData(reg);err!=nil{t.Fatal(err)}
 if _,err:=configdata.NewStore(reg,"../../configs/data").Load(context.Background());err!=nil{t.Fatal(err)}
 f.grant(t,1003,1);f.mustEquip(t,1003);p:=withAttributeEquipmentFinal(t,f)
 if p.HP!=105||p.Attack!=25||p.Power!=60{t.Fatalf("default data %+v",p)}
}
func TestGeneratedAttributeOriginalHookOrder(t *testing.T){
 originalHookCalls.Store(0);f:=newAttributeFixture(t)
 if _,err:=f.exp(100);err!=nil{t.Fatal(err)}
 if originalHookCalls.Load()!=1{t.Fatal("missing named hook")}
 r:=f.get(t);if r.HP!=110{t.Fatalf("growth %+v",r)}
 before:=f.snapshot(t);if _,err:=f.exp(100);err==nil{t.Fatal("original hook failure swallowed")}
 if originalHookCalls.Load()!=1||!reflect.DeepEqual(before,f.snapshot(t)){t.Fatal("original failure continued hooks or changed facts")}
}
`
