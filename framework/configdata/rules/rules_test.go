package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func rows(t *testing.T, body string) []map[string]json.RawMessage {
	t.Helper()
	payload, err := Document([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Rows(payload, false)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCheckNamesTableRowKeyFieldAndRule(t *testing.T) {
	declared := []Rule{
		{Field: "id", Required: true, Unique: true},
		{Field: "name", Required: true},
		{Field: "level", Min: "1"},
		{Field: "kind", Enum: []string{"melee", "ranged"}},
		{Field: "rank", Enum: []string{"1", "2"}},
	}
	key := func(data []map[string]json.RawMessage) func(int) string {
		return func(i int) string { return Canonical(data[i]["id"]) }
	}
	cases := []struct {
		label, body string
		want        Error
	}{
		{"missing", `[{"id":1,"name":"a"},{"id":2}]`, Error{Table: "monster", Row: 2, Key: "2", Field: "name", Rule: "required"}},
		{"null", `[{"id":1,"name":null}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "name", Rule: "required"}},
		{"unique across spellings", `[{"id":1,"name":"a"},{"id":1.0,"name":"b"}]`, Error{Table: "monster", Row: 2, Key: "1", Field: "id", Rule: "unique"}},
		{"below min", `[{"id":1,"name":"a","level":0}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "level", Rule: "min"}},
		{"min on a string", `[{"id":1,"name":"a","level":"3"}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "level", Rule: "min"}},
		{"enum", `[{"id":1,"name":"a","kind":"magic"}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "kind", Rule: "enum"}},
		{"numeric enum", `[{"id":1,"name":"a","rank":3}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "rank", Rule: "enum"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			data := rows(t, testCase.body)
			err := Check("monster", data, declared, key(data))
			var got *Error
			if !errors.As(err, &got) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if got.Table != testCase.want.Table || got.Row != testCase.want.Row || got.Key != testCase.want.Key || got.Field != testCase.want.Field || got.Rule != testCase.want.Rule {
				t.Fatalf("got %+v, want %+v", *got, testCase.want)
			}
			if !strings.Contains(err.Error(), "table monster row") || !strings.Contains(err.Error(), "field "+testCase.want.Field+": "+testCase.want.Rule) {
				t.Fatalf("message %q does not name table / row / field / rule", err)
			}
		})
	}
	// Valid rows, optional columns absent or null, numeric enum spelled 2.0.
	ok := rows(t, `{"rows":[{"id":1,"name":"a","level":1,"kind":"melee","rank":2.0},{"id":2,"name":"b","level":null}]}`)
	if err := Check("monster", ok, declared, nil); err != nil {
		t.Fatalf("valid rows rejected: %v", err)
	}
	// Keys are case-sensitive: "Name" is not the name column (CheckKeys names
	// the misspelling itself).
	if err := Check("monster", rows(t, `[{"id":1,"Name":"a"}]`), declared, nil); err == nil || !strings.Contains(err.Error(), "field name: required") {
		t.Fatalf("Name accepted as name: %v", err)
	}
}

func TestCheckObjectHasNoRowNumber(t *testing.T) {
	payload, err := Document([]byte(`{"data":{"width":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Rows(payload, true)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckObject("world", data[0], []Rule{{Field: "width", Min: "1"}, {Field: "height", Required: true}})
	if err == nil || err.Error() != "table world field width: min: value 0 is below min=1" {
		t.Fatalf("err = %v", err)
	}
}

func TestDocumentRefusesVanishingAndAmbiguousData(t *testing.T) {
	for body, want := range map[string]string{
		"":                          "empty or null",
		"  null ":                   "empty or null",
		`{"rows":null}`:             `wrapper key "rows" is null`,
		`{"rows":[],"data":[]}`:     "ambiguous",
		`{"rows":[{"id":1}],"x":1}`: "", // not a wrapper: the document itself
		`[{"id":1}]`:                "",
		`{"records":[{"id":1}]}   `: "",
	} {
		_, err := Document([]byte(body))
		if want == "" {
			if err != nil {
				t.Errorf("%q: %v", body, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

func TestRuleValidateRejectsBadDeclarations(t *testing.T) {
	for _, rule := range []Rule{
		{},
		{Field: "a", Min: "one"},
		{Field: "a", Enum: []string{"x", ""}},
		{Field: "a", Enum: []string{"x", "x"}},
	} {
		if err := rule.Validate(); err == nil {
			t.Errorf("%+v accepted", rule)
		}
	}
}

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
