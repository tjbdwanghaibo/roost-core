package tablegen

// B10（维护者决定，2026-10-06）：规则统一由运行时加载层强制，生成期检查只作提前反馈，
// 两处用同一份规则声明，不各写一份。旧实现里 schema 标签的 required / unique / min 只在
// CSV 转 JSON（以及 -check）时由 tablegen 自己的 validateRows 检查，生成的 loader 只带
// ref（RR-20261005-NC-75），直接改 configs/data 再 reload 时这三条一条都不查。
// 承诺：一个标签同时驱动生成期检查与生成的 loader——改一处标签，两处一起变。

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOneTagDrivesTheGenerationCheckAndTheGeneratedLoader(t *testing.T) {
	for _, minimum := range []string{"1", "5"} {
		meta := monsterMeta()
		meta.Alias, meta.ImportPath, meta.JSON = "meta0", "example.com/game/configs/schema", "monster.json"
		meta.Fields[2].Min = minimum // level

		// Generation time: a sheet whose level is below the tag's min fails.
		below := map[string]string{"1": "0", "5": "3"}[minimum]
		if _, err := readCSVRecords(writeCSV(t, "id,name,level,code\n1,slime,"+below+",a\n"), meta); err == nil || !strings.Contains(err.Error(), "min") {
			t.Fatalf("min=%s: CSV with level %s: err = %v, want a min violation", minimum, below, err)
		}

		// Run time: the generated loader carries the same rule for configdata
		// to enforce on every load and reload.
		out := t.TempDir()
		if err := generateGo([]Meta{meta}, out, "generated", true, io.Discard); err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(filepath.Join(out, "gen_table_config.go"))
		if err != nil {
			t.Fatal(err)
		}
		if want := `{Field: "level", Min: "` + minimum + `"}`; !strings.Contains(string(source), want) {
			t.Fatalf("min=%s: generated loader does not carry %s:\n%s", minimum, want, source)
		}
	}
}
