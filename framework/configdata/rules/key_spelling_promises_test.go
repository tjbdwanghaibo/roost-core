package rules

import (
	"encoding/json"
	"errors"
	"testing"
)

// configdata 大小写敏感（维护者 2026-10-06）：键必须与声明的字段名逐字一致。
// 这里是加载层与生成器共用的那一条规则（MisspelledKey / CheckKeys / CheckObjectKeys）。
// 它取代了 5a3c4a60 第 5 项的做法——Rows 在一行的几种大小写拼写里按文档顺序只留最后
// 一个、Lookup 大小写不敏感地取值，以便规则查到 encoding/json 解出的那个值；现在这样
// 的行整行被拒绝，Rows 不再预处理，Lookup 逐字匹配。
func TestCheckKeysRejectsCaseVariants(t *testing.T) {
	declared := []string{"id", "level", "name"}
	for body, want := range map[string]Error{
		`[{"id":1,"Level":1}]`:                     {Table: "monster", Row: 1, Key: "k0", Field: "level", Rule: "case"},
		`[{"id":1,"level":5,"Level":0}]`:           {Table: "monster", Row: 1, Key: "k0", Field: "level", Rule: "case"},
		`[{"id":1,"Level":0,"level":5}]`:           {Table: "monster", Row: 1, Key: "k0", Field: "level", Rule: "case"},
		`[{"id":1},{"id":2,"NAME":"a","ID":3}]`:    {Table: "monster", Row: 2, Key: "k1", Field: "id", Rule: "case"},
		`[{"id":1,"LeVeL":0,"Level":5,"LEVEL":0}]`: {Table: "monster", Row: 1, Key: "k0", Field: "level", Rule: "case"},
	} {
		raw, err := Rows([]byte(body), false)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckKeys("monster", raw, declared, func(i int) string { return "k" + string(rune('0'+i)) })
		var got *Error
		if !errors.As(err, &got) || got.Table != want.Table || got.Row != want.Row || got.Key != want.Key || got.Field != want.Field || got.Rule != want.Rule {
			t.Fatalf("%s: err = %v, want %+v", body, err, want)
		}
	}
	// 精确键与未声明的键（与声明名都不只差大小写）通过。
	raw, err := Rows([]byte(`[{"id":1,"level":2,"name":"a","note":"x","Remark":1}]`), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckKeys("monster", raw, declared, nil); err != nil {
		t.Fatalf("exact / undeclared keys rejected: %v", err)
	}
}

func TestCheckObjectKeysNamesTheSpelling(t *testing.T) {
	raw, err := Rows([]byte(`{"width":3,"Width":0}`), true)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckObjectKeys("world", raw[0], []string{"width"})
	if err == nil || err.Error() != `table world field width: case: key "Width" must be spelled "width" (keys are case-sensitive)` {
		t.Fatalf("err = %v", err)
	}
}

// Lookup 逐字匹配：只差大小写的键不是这一列。
func TestLookupIsExact(t *testing.T) {
	row := map[string]json.RawMessage{"Level": json.RawMessage(`1`), "level": json.RawMessage(`2`)}
	if value, ok := Lookup(row, "level"); !ok || string(value) != "2" {
		t.Fatalf("Lookup(level) = %s, %v", value, ok)
	}
	if value, ok := Lookup(map[string]json.RawMessage{"Level": json.RawMessage(`1`)}, "level"); ok {
		t.Fatalf("Lookup(level) found %s under Level", value)
	}
}
