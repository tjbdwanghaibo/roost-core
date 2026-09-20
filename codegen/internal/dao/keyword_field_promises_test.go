package dao

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0246：字段名首字母小写之后可能是 Go 关键字。
//
// 生成物把每个字段的私有名用 `fieldVar` 算出来（`Type` → `type`），于是一个叫
// `Type` 的字段生成的是 `type int32` —— 整个包编译不过，而错误信息是
// `expected '}', found 'type'`，指向生成的临时文件，跟"你的定义里有个叫 Type
// 的字段"之间没有任何提示。
//
// `Type`、`Range`、`Map`、`Func` 都是 DAO 定义里很自然的字段名（timer 节点的
// Type、技能的 Range），所以这不是理论问题：game-demo 第十六批加 World 计时器
// 节点时当场撞上。

func generateKeywordDao(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thing.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Run([]string{"-def", dir, "-out", out, "-pkg", "db"}, os.Stderr); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return out
}

func TestAFieldWhoseLowercaseNameIsAKeywordGenerates(t *testing.T) {
	out := generateKeywordDao(t, `package def

//roost:dao coll=things db=game
type ThingDao struct {
	Type  int32  `+"`bson:\"type\"`"+`
	Range int64  `+"`bson:\"range\"`"+`
	Map   string `+"`bson:\"map\"`"+`
	Name  string `+"`bson:\"name\"`"+`
}
`)
	generated, err := os.ReadFile(filepath.Join(out, "gen_thing_dao.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(generated)
	// The accessors keep the field's own name; only the private storage is
	// renamed, so a definition does not change meaning to its callers.
	for _, want := range []string{"func (d *ThingDao) GetType() int32", "func (d *ThingDao) SetType(", "func (d *ThingDao) GetRange() int64"} {
		if !strings.Contains(body, want) {
			t.Errorf("the generated DAO lost %q:\n%s", want, body)
		}
	}
}

// The same name inside a nested struct, which is where the demo hit it: the
// nested generator has its own template and its own copy of the problem.
func TestANestedFieldWhoseLowercaseNameIsAKeywordGenerates(t *testing.T) {
	out := generateKeywordDao(t, `package def

//roost:dao coll=things db=game
type ThingDao struct {
	Node Node `+"`bson:\"node\"`"+`
}

type Node struct {
	Type   int32  `+"`bson:\"type\"`"+`
	Select string `+"`bson:\"select\"`"+`
}
`)
	generated, err := os.ReadFile(filepath.Join(out, "gen_node_nested.go"))
	if err != nil {
		t.Fatal(err)
	}
	if body := string(generated); !strings.Contains(body, "func (s *Node) GetType() int32") {
		t.Errorf("the generated nested struct lost its accessor:\n%s", body)
	}
}
