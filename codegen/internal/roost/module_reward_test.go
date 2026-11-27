package roost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 独立的真实消费工程：安装不引入资产组件，工程替换发奖表后无须改模块，重装不改文件。
func TestModuleRewardStandaloneAndCustomTable(t *testing.T) {
	root, roost, _ := moduleProject(t, "rewarduser")
	before := moduleProjectDigest(t, root)
	roost("module", "plan", "reward", "--json")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("planning reward wrote files")
	}
	roost("module", "add", "reward")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Modules["reward"]; !ok || len(m.Modules) != 1 {
		t.Fatalf("reward should be the only business module: %v", m.Modules)
	}
	if !contains(m.Services["game"].Mods, "configdata") {
		t.Fatal("generated game service has no reward configuration loader")
	}
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("reward verify: %v, report=%+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"reward": 9})
	before = moduleProjectDigest(t, root)
	roost("module", "add", "reward")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeated reward install changed files")
	}
	for name, data := range map[string]string{
		"reward_group": "id,draws,replacement\n301,0,false\n",
		"reward_entry": "id,group_id,item_id,count,kind,chance_bp,weight\n9,301,9876,2,fixed,0,0\n7,301,9876,3,fixed,0,0\n8,301,2001,1,chance,10000,0\n",
	} {
		if err := os.WriteFile(filepath.Join(root, "configs", "table", name+".csv"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	roost("generate")
	roost("generate", "--check")
	if err := os.WriteFile(filepath.Join(root, "game", "modules", "reward", "custom_reward_consumer_test.go"), []byte(rewardCustomConsumer), 0600); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, root, "test", "-race", "-count=1", "./game/modules/reward")
}

const rewardCustomConsumer = `package reward_test
import (
 "context"
 "slices"
 "testing"
 generated "example.com/studio/rewarduser/configs/generated"
 "example.com/studio/rewarduser/game/modules/reward"
 "github.com/tjbdwanghaibo/roost-core/framework/configdata"
)
func TestGeneratedRewardPublicAPIReadsCustomCSV(t *testing.T) {
 old := configdata.DefaultStore(); t.Cleanup(func(){configdata.SetDefaultStore(old)})
 reg := configdata.NewRegistry(); if err:=generated.RegisterGeneratedConfigData(reg);err!=nil{t.Fatal(err)}
 if _,err:=configdata.NewStore(reg,"../../../configs/data").Load(context.Background());err!=nil{t.Fatal(err)}
 want:=[]reward.Item{{ItemID:2001,Count:1},{ItemID:9876,Count:5}}
 got,err:=reward.Roll(301); if err!=nil || !slices.Equal(got,want){t.Fatalf("custom reward: got %v, err %v, want %v",got,err,want)}
 got[0].Count=900
 got,err=reward.Roll(301); if err!=nil || !slices.Equal(got,want){t.Fatalf("consumer can change next result: %v %v",got,err)}
 if got,err:=reward.Roll(302);err==nil||got!=nil{t.Fatalf("unknown group produced %v, %v",got,err)}
}
`
