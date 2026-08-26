// configgen 演示"meta 文件定义 + 全量代码生成"的配置管线：
// configs/schema/cfg.yaml 是唯一手写的定义，cfg/cfg_gen.go 由
// roost-codegen 的 cfggen 生成（struct、注册、类型化访问器）。
//
// 运行：go run ./configgen
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/tjbdwanghaibo/cube-core/configdata"
	"github.com/tjbdwanghaibo/cube-core/examples/configgen/cfg"
)

func main() {
	// 数据拷到临时目录，便于演示热更（不污染示例数据）。
	dir, err := os.MkdirTemp("", "configgen-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for _, name := range []string{"drop.json", "monster.json", "world.json"} {
		raw, err := os.ReadFile(filepath.Join("configgen/configs/data", name))
		if err != nil {
			raw, err = os.ReadFile(filepath.Join("configs/data", name))
		}
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			log.Fatal(err)
		}
	}

	reg := configdata.NewRegistry()
	cfg.MustRegisterGeneratedConfigData(reg) // 业务侧唯一一行接线

	store := configdata.NewStore(reg, dir)
	snap, err := store.Load(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	monsters, _ := cfg.MonsterTableFrom(snap)
	wolf, _ := monsters.Get(1)
	world, _ := cfg.WorldFrom(snap)
	fmt.Printf("monster#1=%s scene_7=%d world=%dx%d hash=%s\n",
		wolf.Name, len(cfg.MonsterBySceneID(snap, 7)), world.Width, world.Height, snap.Hash[:8])

	// ref 校验：drop_id 指向不存在的掉落组，Reload 被拒、旧快照保持生效。
	badRow := `[{"id":1,"name":"wolf","scene_id":7,"drop_id":999}]`
	if err := os.WriteFile(filepath.Join(dir, "monster.json"), []byte(badRow), 0o644); err != nil {
		log.Fatal(err)
	}
	if _, err := store.Reload(context.Background()); err != nil {
		fmt.Printf("dangling ref rejected: %v\n", err)
	}
	fmt.Printf("current snapshot still version=%d\n", store.Current().Version)

	// 正常热更：改名后 Reload，hash 变化、版本递增。
	goodRow := `[{"id":1,"name":"dire wolf","scene_id":7,"drop_id":100}]`
	if err := os.WriteFile(filepath.Join(dir, "monster.json"), []byte(goodRow), 0o644); err != nil {
		log.Fatal(err)
	}
	snap2, err := store.Reload(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	monsters2, _ := cfg.MonsterTableFrom(snap2)
	renamed, _ := monsters2.Get(1)
	fmt.Printf("reloaded: monster#1=%s version=%d hash=%s\n", renamed.Name, snap2.Version, snap2.Hash[:8])
}
