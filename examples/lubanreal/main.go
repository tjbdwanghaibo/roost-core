// lubanreal 是 Luban 的真实接入示例：gen/ 与 configs/data/ 均由官方 luban
// CLI（v4.11.0）从 Defines/*.xml + Datas/ 真实生成（见 gen.sh），
// configdata.RegisterExternalTables 把 Luban 的 Tables 聚合装进 roost 快照
// ——原子热更、回滚、hash、ActiveSnapshot 请求一致性全部继承。
//
// 运行：go run ./lubanreal
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/tjbdwanghaibo/roost-core/configdata"
	cfg "github.com/tjbdwanghaibo/roost-core/examples/lubanreal/gen"
)

func main() {
	dir, err := os.MkdirTemp("", "lubanreal-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	raw, err := os.ReadFile("lubanreal/configs/data/tbitem.json")
	if err != nil {
		raw, err = os.ReadFile("configs/data/tbitem.json")
	}
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tbitem.json"), raw, 0o644); err != nil {
		log.Fatal(err)
	}

	// Luban go-json 的装载约定是 JsonLoader(表名) -> []map[string]any；
	// 适配层只做"从快照数据目录读文件 + 反序列化"这一步。
	reg := configdata.NewRegistry()
	configdata.MustRegisterExternalTables(reg, "luban",
		func(read func(string) ([]byte, error)) (*cfg.Tables, error) {
			return cfg.NewTables(func(table string) ([]map[string]interface{}, error) {
				raw, err := read(table + ".json")
				if err != nil {
					return nil, err
				}
				var rows []map[string]interface{}
				if err := json.Unmarshal(raw, &rows); err != nil {
					return nil, err
				}
				return rows, nil
			})
		})

	store := configdata.NewStore(reg, dir)
	snap, err := store.Load(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	tables, _ := configdata.ExternalTablesFrom[*cfg.Tables](snap, "luban")
	fmt.Printf("items=%d sword=%d\n", len(tables.TbItem.GetDataList()), tables.TbItem.Get(1).Price)

	// 热更：整组 Tables 原子替换；旧快照（在途请求视角）保持不变。
	updated := `[{"id":1,"name":"sword","price":150},{"id":2,"name":"shield","price":80}]`
	if err := os.WriteFile(filepath.Join(dir, "tbitem.json"), []byte(updated), 0o644); err != nil {
		log.Fatal(err)
	}
	snap2, err := store.Reload(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	tables2, _ := configdata.ExternalTablesFrom[*cfg.Tables](snap2, "luban")
	fmt.Printf("reloaded sword=%d, old snapshot sword=%d\n",
		tables2.TbItem.Get(1).Price, tables.TbItem.Get(1).Price)
}
