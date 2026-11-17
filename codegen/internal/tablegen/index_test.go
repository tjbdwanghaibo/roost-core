package tablegen

import (
	"strings"
	"testing"
)

// index 的写法同 cfggen：index:"true" 以 Go 字段名命名，index:"<名字>" 指定名字，index:"false" 没有。
func TestIndexTagNamesTheIndex(t *testing.T) {
	metas := parseSchema(t, "//roost:table\ntype Monster struct { ID int32; SceneID int32 `index:\"true\"`; Kind string `index:\"ByKind\"`; Level int32 `index:\"false\"` }\n")
	got := fieldValues(metas[0].Fields, func(f Field) string { return f.Index })
	if strings.Join(got, ",") != ",SceneID,ByKind," {
		t.Fatalf("index names = %q", got)
	}
	if accessor := indexAccessor(metas[0], metas[0].Fields[1]); accessor != "MonsterBySceneID" {
		t.Fatalf("accessor = %s", accessor)
	}
}

// 每条 index 声明错误各一次。
func TestIndexDeclarationsAreRejected(t *testing.T) {
	cases := []struct{ label, source, want string }{
		{"not an identifier", "//roost:table\ntype M struct { ID int32; S int32 `index:\"by scene\"` }\n", `index:"by scene" must be true, false or a Go identifier`},
		{"on an object", "//roost:object\ntype M struct { S int32 `index:\"true\"` }\n", "index only applies to //roost:table rows"},
		{"on the key", "//roost:table key=ID\ntype M struct { ID int32 `index:\"true\"`; S int32 }\n", "the key already has MByID"},
		{"float field", "//roost:table\ntype M struct { ID int32; Rate float64 `index:\"true\"` }\n", "index needs a string, integer or bool field, got float64"},
		// 具名类型按底层类型看（交接 C5 (a)）：底层是浮点的照样拒绝；别的包里的、底层不是内建标量的，报错说明原因。
		{"named float", "//roost:table\ntype M struct { ID int32; Rate Rate `index:\"true\"` }\ntype Rate float64\n", "got Rate (float64)"},
		{"named type from another package", "//roost:table\ntype M struct { ID int32; D time.Duration `index:\"true\"` }\n", "time.Duration is defined in another package"},
		{"named struct", "//roost:table\ntype M struct { ID int32; R Reward `index:\"true\"` }\ntype Reward struct { ID int32 }\n", "Reward is a named type whose underlying type is not a built-in scalar"},
		{"parser field", "//roost:table\ntype M struct { ID int32; S string `index:\"true\" parser:\"json\"` }\n", "with parser:json"},
		{"duplicate name", "//roost:table\ntype M struct { ID int32; A int32 `index:\"X\"`; B int32 `index:\"X\"` }\n", `index "X" is declared on both A and B`},
		{"collides with the key accessor", "//roost:table key=ID\ntype M struct { ID int32; Code int32 `index:\"ID\"` }\n", "collides with the key accessor"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			root := t.TempDir()
			writeTablegenTestFile(t, root+"/go.mod", "module example.com/index\n")
			writeTablegenTestFile(t, root+"/schema/schema.go", "package schema\n"+testCase.source)
			metas, err := parseMetaRoot(root + "/schema")
			if err == nil {
				err = validateMetas(metas)
			}
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("err = %v, want it to contain %q", err, testCase.want)
			}
		})
	}
}
