package roost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 两个模块在自定义 Go module 的新 game 上直接安装；自带数据能跑，核心事务场景与变异都走正式生成物。
func TestModuleGachaIdleScenariosAndMutations(t *testing.T) {
	root, roost, _ := moduleProject(t, "gachaidle")
	before := moduleProjectDigest(t, root)
	roost("module", "plan", "gacha", "idle", "--json")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("module plan changed the project")
	}
	roost("module", "add", "gacha", "idle")
	report, err := VerifyModules(context.Background(), root, []string{"gacha", "idle"}, nil)
	if err != nil {
		t.Fatalf("verify: %v %+v", err, report)
	}
	want := map[string]int{"gacha": 29, "idle": 19}
	if len(report.Modules) != 2 {
		t.Fatalf("verification rows: %+v", report.Modules)
	}
	for _, result := range report.Modules {
		if result.Passed != want[result.Name] || result.Skipped != 0 || !result.OK {
			t.Fatalf("scenario counts: %+v", result)
		}
	}

	// 常规场景自己带规则；另验安装附带的 CSV 确实可装载并且默认十连能拿到至少一件保底奖励。
	path := filepath.Join(root, "game/handler/gacha_defaults_generated_test.go")
	if err := os.WriteFile(path, []byte(gachaIdleDefaultScenario), 0600); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, root, "test", "-count=1", "-v", "-run", "^TestGeneratedGachaDefaults$", "./game/handler")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// 只接受可编译且命中指定业务断言的红灯；每次自动恢复原文件。
	recipeMutationMustFail(t, root, "game/components/gacha/gacha.go",
		"if misses+1 >= pool.PityAfter {", "if misses+1 > pool.PityAfter {",
		"./game/handler", "TestGachaTenAppliesPityInsideTheSameTransaction", "ten result")
	recipeMutationMustFail(t, root, "game/components/gacha/gacha.go",
		"component.owner.Wallet().Add(-cost)", "component.owner.Wallet().Add(-cost/2)",
		"./game/handler", "TestGachaSingleChargesAndGrants", "single result")
	recipeMutationMustFail(t, root, "game/components/gacha/rules.go",
		"if roll < row.Weight {", "if roll <= row.Weight {",
		"./game/entities/player", "TestGachaWeightsUseExactHalfOpenIntervals", "roll 2")
	recipeMutationMustFail(t, root, "game/components/idle/idle.go",
		"if capped {", "if false && capped {",
		"./game/handler", "TestIdleOfflineCapDropsOldOverflow", "cap")
	recipeMutationMustFail(t, root, "game/components/idle/idle.go",
		"sequence == state.GetSequence()", "false && sequence == state.GetSequence()",
		"./game/handler", "TestIdleDuplicateReturnsOriginalAfterTimeMoves", "idle_sequence")
	roost("generate", "--check")
	before = moduleProjectDigest(t, root)
	roost("module", "add", "gacha", "idle")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("repeated installation changed the project")
	}
}

const gachaIdleDefaultScenario = `package handler
import(
 "context"
 "testing"
 generated "example.com/studio/gachaidle/configs/generated"
 "github.com/tjbdwanghaibo/roost-core/framework/configdata"
)
func TestGeneratedGachaDefaults(t *testing.T){
 f:=newGachaFixture(t,1000)
 registry:=configdata.NewRegistry()
 if err:=generated.RegisterGeneratedConfigData(registry);err!=nil{t.Fatal(err)}
 if _,err:=configdata.NewStore(registry,"../../configs/data").Load(context.Background());err!=nil{t.Fatal(err)}
 r:=f.mustDraw(t,"default-ten",1,10)
 rare:=0
 for _,item:=range r.Items {if item<1001||item>1003 {t.Fatalf("default item %d",item)};if item==1003{rare++}}
 if r.Cost!=100||r.Balance!=900||len(r.Items)!=10||rare<1||r.Misses>=10 {t.Fatalf("default draw %+v",r)}
}
`
