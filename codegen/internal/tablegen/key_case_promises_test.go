package tablegen

// configdata 大小写敏感（维护者 2026-10-06）：生成期检查与加载层用同一条拼写规则
// （configdata/rules.MisspelledKey）。`-check` 拒绝只差大小写的 JSON 键；CSV 转换拒绝只差
// 大小写的表头——旧实现按 csv 名逐字查表头，"Level" 被当成未知列跳过，这一列的值静默丢成零值。

import (
	"strings"
	"testing"
)

func TestCheckJSONRejectsMisspelledKeys(t *testing.T) {
	meta := monsterMeta()
	meta.JSON = "monster.json"
	for body, want := range map[string]string{
		`[{"id":1,"name":"slime","Level":3,"code":"a"}]`:           `table monster row 1 (key 1) field level: case: key "Level" must be spelled "level"`,
		`[{"id":1,"name":"slime","level":3,"Level":0,"code":"a"}]`: `field level: case: key "Level"`,
	} {
		err := checkJSONFiles([]Meta{meta}, writeMonsterJSON(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", body, err, want)
		}
	}
	// 未声明的键维持原行为（-check 不管）。
	if err := checkJSONFiles([]Meta{meta}, writeMonsterJSON(t, `[{"id":1,"name":"slime","level":3,"code":"a","note":"x"}]`)); err != nil {
		t.Fatalf("undeclared key rejected: %v", err)
	}
}

func TestCSVHeaderIsCaseSensitive(t *testing.T) {
	_, err := readCSVRecords(writeCSV(t, "id,name,Level,code\n1,slime,3,a\n"), monsterMeta())
	if err == nil || !strings.Contains(err.Error(), `monster.csv: header "Level" must be spelled "level"`) {
		t.Fatalf("err = %v, want the misspelled header named", err)
	}
	// 未声明的列照旧跳过。
	if _, err := readCSVRecords(writeCSV(t, "id,name,level,code,note\n1,slime,3,a,x\n"), monsterMeta()); err != nil {
		t.Fatalf("undeclared column rejected: %v", err)
	}
}
