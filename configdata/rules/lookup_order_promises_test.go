package rules

import (
	"encoding/json"
	"testing"
)

// 发版前审查观察：一行里同一列有几种大小写拼写时，规则检查的值要与加载层解出的类型化行一致。
//
// 加载层用 encoding/json 把同一份载荷解进行结构体：几个键都落到同一个字段时（精确匹配或大小写不敏感匹配），
// 按文档顺序最后一个生效——精确拼写并不优先（{"level":5,"Level":0} 解出 0）。规则在原始行（map）上查，旧 Lookup
// 先取精确键、否则遍历 map 取第一个大小写变体：精确键在前、变体在后时查的是 5、类型化行是 0，min 规则放过了 0；
// 没有精确键、有几个变体时取哪个随 map 遍历顺序变。修后 Rows 按文档顺序只保留每组大小写变体里最后一个键，
// Lookup 查到的就是类型化行里的值。
func TestRulesCheckTheValueEncodingJSONDecodes(t *testing.T) {
	type row struct {
		ID    int `json:"id"`
		Level int `json:"level"`
	}
	declared := []Rule{{Field: "level", Min: "1"}}
	for _, body := range []string{
		`[{"id":1,"level":5,"Level":0}]`,
		`[{"id":1,"Level":0,"level":5}]`,
		`[{"id":1,"Level":5,"LEVEL":0}]`,
		`[{"id":1,"LEVEL":0,"Level":5}]`,
		`[{"id":1,"LeVeL":0,"Level":5,"LEVEL":0}]`,
		`[{"id":1,"LeVeL":0,"Level":0,"LEVEL":5}]`,
		`[{"id":1,"level":0,"LEVEL":5,"level":0}]`,
		`[{"id":1,"Level":0,"level":5,"Level":0,"level":7}]`,
	} {
		var typed []row
		if err := json.Unmarshal([]byte(body), &typed); err != nil {
			t.Fatal(err)
		}
		wantRefused := typed[0].Level < 1
		// map 的遍历顺序每次不同：同一份载荷重复解析、检查，每次都要与类型化行一致。
		for i := 0; i < 64; i++ {
			raw, err := Rows([]byte(body), false)
			if err != nil {
				t.Fatal(err)
			}
			err = Check("monster", raw, declared, nil)
			if refused := err != nil; refused != wantRefused {
				t.Fatalf("%s: encoding/json decodes level=%d, but the rule check refused=%v (%v) on run %d", body, typed[0].Level, refused, err, i)
			}
		}
	}
}

// 单例对象（CheckObject）走同一条路径。
func TestObjectRulesCheckTheValueEncodingJSONDecodes(t *testing.T) {
	var typed struct {
		Width int `json:"width"`
	}
	body := `{"width":3,"Width":0}`
	if err := json.Unmarshal([]byte(body), &typed); err != nil {
		t.Fatal(err)
	}
	raw, err := Rows([]byte(body), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckObject("world", raw[0], []Rule{{Field: "width", Min: "1"}}); err == nil {
		t.Fatalf("%s decodes width=%d, but the min=1 rule accepted it", body, typed.Width)
	}
}

// 没有大小写冲突的行保持原样：键的拼写、个数不变。
func TestRowsWithoutCaseVariantsAreUnchanged(t *testing.T) {
	raw, err := Rows([]byte(`[{"id":1,"Name":"a","level":2,"kind":"melee"}]`), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw[0]) != 4 || raw[0]["Name"] == nil || raw[0]["level"] == nil {
		t.Fatalf("row = %v, want the four keys as written", raw[0])
	}
}

// 不是 Rows 解出的行（手拼的 map）里仍有几个变体时，Lookup 的选择也是确定的：字节序最小的键。
func TestLookupIsDeterministicOnAHandBuiltRow(t *testing.T) {
	for i := 0; i < 64; i++ {
		row := map[string]json.RawMessage{"Level": json.RawMessage(`1`), "LEVEL": json.RawMessage(`2`), "LeVeL": json.RawMessage(`3`)}
		value, ok := Lookup(row, "level")
		if !ok || string(value) != "2" {
			t.Fatalf("Lookup = %s, %v on run %d; want LEVEL's value every time", value, ok, i)
		}
	}
}
