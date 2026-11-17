package roost

import (
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// moduleConfigFixtureName 是模块场景测试的公共夹具：安装器在每个放了模块测试文件（含联动的测试文件）的包里
// 写一份（已经有了就不动），让这个包的测试开始之前先按 game 进程正式启动的方式装载工程的 configs/data。
//
// 维护者 2026-11-17（PLAN-2026-11-17-r9-genre-modules §9.5）：工程挂在模块流程上的业务会读工程的配置表——
// 卡牌、放置在 enter 的 EnterFirst 监听里发道具表里的道具；模块场景不装工程的表时 bag 认不出这些道具号，
// enter 有 13 条答 bag_item_invalid。夹具放在包级、由安装器统一写，不在每个模块的测试里各装一遍。
const moduleConfigFixtureName = "roost_config_data_test.go"

// moduleConfigFixtures 是这次安装要新写的公共夹具（工程内相对路径，排序）：files 里的测试文件所在的包，
// 去掉已经有夹具的。
func moduleConfigFixtures(root string, files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, rel := range files {
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		fixture := path.Join(path.Dir(rel), moduleConfigFixtureName)
		if seen[fixture] {
			continue
		}
		seen[fixture] = true
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(fixture))); err == nil {
			continue
		}
		out = append(out, fixture)
	}
	sort.Strings(out)
	return out
}

// writeModuleConfigFixtures 写 moduleConfigFixtures 算出的夹具。包名取同目录里已经写好的模块测试文件
// （外部测试包 xxx_test 取 xxx：夹具是包内测试文件，两种测试都会先跑它的 init）。在 AddModules 的回滚边界内调用。
func writeModuleConfigFixtures(root, goModule string, fixtures, files []string) error {
	for _, fixture := range fixtures {
		dir := path.Dir(fixture)
		pkg := ""
		for _, rel := range files {
			if strings.HasSuffix(rel, "_test.go") && path.Dir(rel) == dir {
				file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, parser.PackageClauseOnly)
				if err != nil {
					return fmt.Errorf("config fixture %s: %w", fixture, err)
				}
				pkg = strings.TrimSuffix(file.Name.Name, "_test")
				break
			}
		}
		if pkg == "" {
			return fmt.Errorf("config fixture %s: no module test file in %s", fixture, dir)
		}
		body, err := format.Source([]byte(moduleConfigFixtureSource(pkg, goModule, dir)))
		if err != nil {
			return fmt.Errorf("config fixture %s: %w", fixture, err)
		}
		if err := writeAtomic(filepath.Join(root, filepath.FromSlash(fixture)), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// moduleConfigFixtureSource 是夹具的源码。dir 是包在工程里的目录：go test 的工作目录是包目录，
// configs/data 按它到工程根的相对路径找（正式启动时 config_data.dir 的缺省值 configs/data 按工程根解析）。
func moduleConfigFixtureSource(pkg, goModule, dir string) string {
	parts := []string{}
	for range strings.Split(dir, "/") {
		parts = append(parts, strconv.Quote(".."))
	}
	parts = append(parts, strconv.Quote("configs"), strconv.Quote("data"))
	return `package ` + pkg + `

import (
	"context"
	"fmt"
	"path/filepath"

	generated "` + goModule + `/configs/generated"

	"github.com/tjbdwanghaibo/roost-core/framework/configdata"
)

// 模块场景测试的公共夹具（roost module add 写入，之后归工程所有）：本包的测试开始之前，按 game 进程正式启动的
// 方式装载工程的配置表——生成的 RegisterGeneratedConfigData 登记工程的全部表，再从 configs/data（config_data.dir
// 的缺省值，按工程根目录解析）装载一次。装载把它发布成进程当前的配置（缺省 Store 与新请求钉住的快照），
// 所以工程挂在模块流程上的业务（例如 EnterFirst 监听里发道具表里的道具）在模块场景里读到的是工程的表，与线上一致。
// 用例自己装一张测试表的照旧装；结束时换回的就是这里装载的工程配置。
//
// 表登记在新建的 Registry 上而不是 configdata.DefaultRegistry：同一个测试进程里别的代码可能已经往缺省 Registry
// 登记过，重复登记会报错；登记的表与正式启动相同。装载失败（configs/data 与生成的登记不一致）时正式启动同样会失败，
// 这里直接让本包的测试失败：先 roost generate。
func init() {
	registry := configdata.NewRegistry()
	if err := generated.RegisterGeneratedConfigData(registry); err != nil {
		panic(fmt.Sprintf("register the project's config tables: %v", err))
	}
	if _, err := configdata.NewStore(registry, filepath.Join(` + strings.Join(parts, ", ") + `)).Load(context.Background()); err != nil {
		panic(fmt.Sprintf("load the project's configs/data the way the game process starts (run roost generate): %v", err))
	}
}
`
}
