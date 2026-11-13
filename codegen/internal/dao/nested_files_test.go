package dao

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDefFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func nestedNames(defs *Definitions) string {
	names := make([]string, 0, len(defs.Nested))
	for _, nested := range defs.Nested {
		names = append(names, nested.Name)
	}
	return strings.Join(names, ",")
}

// 嵌套类型可以定义在 db/def 的任何文件里，与文件名的字典序无关。
// 修前：定义在排在 DAO 文件之前的文件里的嵌套类型识别不到（每个文件只看自己与之前文件里的 DAO、
// 只认本文件里的结构体），生成时报 unsupported sync type；排在之后的文件才认得出。
// 业务模块把自己的状态类型放在自己的文件里（checkin.go 排在 player.go 之前），正好撞上。
func TestNestedTypesAreFoundWhateverFileDefinesThem(t *testing.T) {
	const dao = "package def\n\n//roost:dao coll=players db=main\ntype PlayerDao struct {\n" +
		"\tEarly EarlyState `store:\"early\" dao:\"persist,sync\"`\n" +
		"\tLate  LateState  `store:\"late\" dao:\"persist,sync\"`\n}\n"
	dir := writeDefFiles(t, map[string]string{
		"player.go": dao,
		// 排在 player.go 之前；它还引用了排在最后的文件里的类型。
		"a_early.go": "package def\n\ntype EarlyState struct {\n\tCount int64 `store:\"1\"`\n\tInner *DeepItem `store:\"2\"`\n}\n",
		"z_late.go":  "package def\n\ntype LateState struct {\n\tCount int64 `store:\"1\"`\n}\n\ntype DeepItem struct {\n\tLevel int32 `store:\"1\"`\n}\n",
	})
	defs, err := parseDefDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// LateState 由逐文件那一遍认出（它的文件排在 DAO 之后），其余两个由全部解析完之后的那一遍补上。
	if got := nestedNames(defs); got != "LateState,EarlyState,DeepItem" {
		t.Fatalf("nested types = %q, want LateState,EarlyState,DeepItem", got)
	}
	if _, _, err := buildSyncSchema(defs, "db"); err != nil {
		t.Fatalf("sync schema: %v", err)
	}
}

// 同步脏位是 64 位掩码、每个顶层字段一位：第 65 个字段在生成期拒绝并说明原因。
// 修前：生成成功，生成物编译失败（constant overflows uint64），报错不指向定义。
func TestDaoWithMoreThan64FieldsIsRefused(t *testing.T) {
	var fields strings.Builder
	for i := 0; i < 65; i++ {
		fmt.Fprintf(&fields, "\tF%d int64 `store:\"f%d\" dao:\"nosync\"`\n", i, i)
	}
	dir := writeDefFiles(t, map[string]string{"wide.go": "package def\n\n//roost:dao coll=wide db=main\ntype WideDao struct {\n" + fields.String() + "}\n"})
	_, err := parseDefDir(dir)
	if err == nil || !strings.Contains(err.Error(), "WideDao has 65 fields") || !strings.Contains(err.Error(), "nested struct") {
		t.Fatalf("parse = %v, want a refusal that names the 64-field limit and the nested-struct way out", err)
	}
	// 恰好 64 个仍然可以。
	trimmed := strings.Replace(fields.String(), "\tF64 int64 `store:\"f64\" dao:\"nosync\"`\n", "", 1)
	dir = writeDefFiles(t, map[string]string{"wide.go": "package def\n\n//roost:dao coll=wide db=main\ntype WideDao struct {\n" + trimmed + "}\n"})
	if _, err := parseDefDir(dir); err != nil {
		t.Fatalf("64 fields: %v", err)
	}
}
