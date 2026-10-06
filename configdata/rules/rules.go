// Package rules 是配置数据的列规则：声明（Rule）与检查（Check）只有这一份，
// 运行时加载层（configdata，每次 Load / Reload）和生成器（tablegen 的 CSV 转换与
// -check）共用（维护者决定 B10，2026-10-06）。生成期检查只是提前反馈，真正把关的
// 是加载层；两处跑的是同一段代码、同一组规则，改一处规则两处一起变。
//
// 键的拼写也是这里的一条规则（MisspelledKey）：数据文件里的键与声明的字段名逐字
// 一致，只差大小写的键被拒绝，不交给 encoding/json 的大小写不敏感匹配。
//
// 规则在原始 JSON 行上检查，所以“缺列”和“零值”分得清——configdata 只拿类型化
// 的行时做不到，这正是 required 以前在运行时查不了的原因（RR-20261005-NC-75）。
// Ref 需要目标表，由加载层在全部表加载后用类型化的表检查，Check 跳过它。
//
// 本包只依赖标准库：codegen 层不得 import 运行时，这里是根包边界测试里唯一的
// 例外，保持叶子包是这个例外成立的前提。
package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Rule 是一列的规则。零值 Rule 不约束任何东西。
type Rule struct {
	// Field 是数据文件里的 JSON 键，逐字匹配（大小写敏感）。
	Field string
	// Required：每行都必须出现且不为 null。
	Required bool
	// Unique：任两行的值不相同；缺省或 null 的行不参与比较。
	Unique bool
	// Min 是十进制下限（含），"" 表示无；值必须是 JSON 数字。
	Min string
	// Ref 是目标表名：非零值必须是该表的主键（Required 时零值也要是）。
	// 由加载层检查，Check 不看它。
	Ref string
	// Enum 是允许的取值（字符串形式：字符串原样，数字取规范十进制，
	// bool 为 true / false），空表示不限。
	Enum []string
}

// Validate 检查规则声明本身：字段名非空、Min 是数字、Enum 不含空串与重复项。
func (r Rule) Validate() error {
	if r.Field == "" {
		return errors.New("rule has no field")
	}
	if r.Min != "" {
		if _, err := strconv.ParseFloat(r.Min, 64); err != nil {
			return fmt.Errorf("field %s: min=%q is not a number", r.Field, r.Min)
		}
	}
	seen := make(map[string]bool, len(r.Enum))
	for _, value := range r.Enum {
		if value == "" {
			return fmt.Errorf("field %s: enum has an empty value", r.Field)
		}
		if seen[value] {
			return fmt.Errorf("field %s: enum repeats %q", r.Field, value)
		}
		seen[value] = true
	}
	return nil
}

// Error 点名一次违反：哪张表、哪一行（1 起；对象为 0）、该行的主键（已知时）、
// 哪个字段、哪条规则。
type Error struct {
	Table  string
	Row    int
	Key    string
	Field  string
	Rule   string // required / unique / min / enum / ref / case
	Detail string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("table ")
	b.WriteString(e.Table)
	if e.Row > 0 {
		fmt.Fprintf(&b, " row %d", e.Row)
		if e.Key != "" {
			fmt.Fprintf(&b, " (key %s)", e.Key)
		}
	}
	if e.Field != "" {
		b.WriteString(" field ")
		b.WriteString(e.Field)
	}
	fmt.Fprintf(&b, ": %s: %s", e.Rule, e.Detail)
	return b.String()
}

// Document 返回数据文件的载荷：文件本身，或者当文件恰好是只含 rows / records /
// data 其中一个键的对象时，该键的值。空文件、null、null 包装键、多个包装键并存
// 都是错误——一份数据整体消失或含义不明不能被静默接受。
func Document(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, errors.New("empty or null document")
	}
	if trimmed[0] != '{' {
		return trimmed, nil
	}
	// 显式识别包装，而不是“第一次解码失败再试包装”：对象目标宽松解码
	// {"data":{...}} 会成功成全零值，包装分支永远走不到。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return trimmed, nil // 让调用方的解码报出真正的语法错误
	}
	wrappers := 0
	var candidate json.RawMessage
	for _, key := range []string{"rows", "records", "data"} {
		value, ok := probe[key]
		if !ok {
			continue
		}
		if string(bytes.TrimSpace(value)) == "null" {
			return nil, fmt.Errorf("wrapper key %q is null", key)
		}
		wrappers++
		if candidate == nil {
			candidate = value
		}
	}
	if wrappers > 1 {
		return nil, errors.New("multiple wrapper keys (rows/records/data) present — ambiguous document")
	}
	if wrappers == 1 && len(probe) == 1 {
		return bytes.TrimSpace(candidate), nil
	}
	return trimmed, nil
}

// Rows 把载荷解成原始行：表是行的列表，对象是唯一的一行。
func Rows(payload []byte, object bool) ([]map[string]json.RawMessage, error) {
	if object {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(payload, &row); err != nil {
			return nil, err
		}
		return []map[string]json.RawMessage{row}, nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(payload, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// MisspelledKey 是键的拼写规则：键必须与声明的字段名逐字一致（大小写敏感）。它按
// keys 的顺序找第一个不是声明名、却与某个声明名只差大小写的键，返回它和应有的拼写。
// 与所有声明名都不只差大小写的键是未声明的键，不归这条规则管（加载层宽松模式忽略、
// 严格模式由解码拒绝）。一行里同一字段写了几种大小写时，至多一个是逐字的拼写，其余
// 都在这里被找出来，所以“几种拼写取哪一个”不会出现。
//
// encoding/json 匹配键时大小写不敏感，"Level" 会静默填进 json 名为 level 的字段；
// 加载层（configdata）在解码后、生成器（tablegen 的 -check 与 CSV 表头）在转换时都用
// 这一条规则核对（维护者 2026-10-06：“configdata 需要大小写敏感”）。
func MisspelledKey(keys, declared []string) (key, want string, found bool) {
	for _, key := range keys {
		if slices.Contains(declared, key) {
			continue
		}
		for _, name := range declared {
			if strings.EqualFold(key, name) {
				return key, name, true
			}
		}
	}
	return "", "", false
}

// CaseError 点名一处拼写违反：table 的第 row 行（1 起；对象为 0，rowKey 是该行主键，
// 未知时为空）里的 key 应拼作 want。field 是应有拼写在行里的位置：顶层就是 want，
// 嵌套时形如 rewards[1].item_id。
func CaseError(table string, row int, rowKey, field, key, want string) *Error {
	return &Error{Table: table, Row: row, Key: rowKey, Field: field, Rule: "case",
		Detail: fmt.Sprintf("key %q must be spelled %q (keys are case-sensitive)", key, want)}
}

// CheckKeys 对一张表的原始行执行拼写规则，declared 是行的字段名（json 名）。
// key 给出第 i 行（0 起）的主键用于点名，可以为 nil。第一处违反即返回 *Error。
func CheckKeys(table string, rows []map[string]json.RawMessage, declared []string, key func(row int) string) error {
	for index, row := range rows {
		if name, field, found := MisspelledKey(slices.Sorted(maps.Keys(row)), declared); found {
			rowKey := ""
			if key != nil {
				rowKey = key(index)
			}
			return CaseError(table, index+1, rowKey, field, name, field)
		}
	}
	return nil
}

// CheckObjectKeys 对单例配置执行同样的拼写规则；错误里不带行号。
func CheckObjectKeys(table string, row map[string]json.RawMessage, declared []string) error {
	if name, field, found := MisspelledKey(slices.Sorted(maps.Keys(row)), declared); found {
		return CaseError(table, 0, "", field, name, field)
	}
	return nil
}

// Check 对一张表的原始行执行 required / unique / min / enum（Ref 由加载层查）。
// key 给出第 i 行（0 起）的主键用于点名，可以为 nil。第一处违反即返回 *Error。
func Check(table string, rows []map[string]json.RawMessage, rules []Rule, key func(row int) string) error {
	return check(table, rows, rules, key, false)
}

// CheckObject 对单例配置（一个 JSON 对象）执行同样的规则；错误里不带行号。
func CheckObject(table string, row map[string]json.RawMessage, rules []Rule) error {
	return check(table, []map[string]json.RawMessage{row}, rules, nil, true)
}

func check(table string, rows []map[string]json.RawMessage, rules []Rule, key func(row int) string, object bool) error {
	for _, rule := range rules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("config table %s: %w", table, err)
		}
		var minimum float64
		if rule.Min != "" {
			minimum, _ = strconv.ParseFloat(rule.Min, 64)
		}
		seen := make(map[string]int)
		for index, row := range rows {
			fail := func(name, detail string) error {
				e := &Error{Table: table, Field: rule.Field, Rule: name, Detail: detail}
				if !object {
					e.Row = index + 1
					if key != nil {
						e.Key = key(index)
					}
				}
				return e
			}
			value, present := Lookup(row, rule.Field)
			if !present || isNull(value) {
				if rule.Required {
					return fail("required", "missing or null")
				}
				continue
			}
			if rule.Unique {
				canonical := Canonical(value)
				if first, dup := seen[canonical]; dup {
					return fail("unique", fmt.Sprintf("value %s repeats row %d", canonical, first))
				}
				seen[canonical] = index + 1
			}
			if rule.Min != "" {
				number, ok := numberOf(value)
				if !ok {
					return fail("min", fmt.Sprintf("value %s is not a number", Canonical(value)))
				}
				if number < minimum {
					return fail("min", fmt.Sprintf("value %s is below min=%s", Canonical(value), rule.Min))
				}
			}
			if len(rule.Enum) > 0 {
				canonical, ok := scalar(value)
				if !ok || !contains(rule.Enum, canonical) {
					return fail("enum", fmt.Sprintf("value %s is not one of [%s]", Canonical(value), strings.Join(rule.Enum, " ")))
				}
			}
		}
	}
	return nil
}

// Lookup 在一行里找 field 列，键逐字匹配（大小写敏感，见 MisspelledKey）。
func Lookup(row map[string]json.RawMessage, field string) (json.RawMessage, bool) {
	value, ok := row[field]
	return value, ok
}

// Canonical 是一个 JSON 值用于比较与报错的规范形式：字符串取原文，数字取规范
// 十进制（1、1.0、1e0 相同），其余取紧凑 JSON。
func Canonical(value json.RawMessage) string {
	if text, ok := scalar(value); ok {
		return text
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, value); err != nil {
		return string(value)
	}
	return buf.String()
}

func isNull(value json.RawMessage) bool {
	return string(bytes.TrimSpace(value)) == "null"
}

// scalar 返回字符串 / 数字 / bool 的规范字符串形式。
func scalar(value json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return "", false
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", false
		}
		return text, true
	case 't', 'f':
		var flag bool
		if err := json.Unmarshal(trimmed, &flag); err != nil {
			return "", false
		}
		return strconv.FormatBool(flag), true
	case '{', '[', 'n':
		return "", false
	}
	text := string(trimmed)
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return strconv.FormatInt(n, 10), true
	}
	if n, err := strconv.ParseUint(text, 10, 64); err == nil {
		return strconv.FormatUint(n, 10), true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return "", false
	}
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return strconv.FormatInt(int64(f), 10), true
	}
	return strconv.FormatFloat(f, 'g', -1, 64), true
}

func numberOf(value json.RawMessage) (float64, bool) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || trimmed[0] == '"' || trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == 't' || trimmed[0] == 'f' || trimmed[0] == 'n' {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(trimmed), 64)
	return f, err == nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
