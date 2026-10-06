// Package rules 是配置数据的列规则：声明（Rule）与检查（Check）只有这一份，
// 运行时加载层（configdata，每次 Load / Reload）和生成器（tablegen 的 CSV 转换与
// -check）共用（维护者决定 B10，2026-10-06）。生成期检查只是提前反馈，真正把关的
// 是加载层；两处跑的是同一段代码、同一组规则，改一处规则两处一起变。
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
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Rule 是一列的规则。零值 Rule 不约束任何东西。
type Rule struct {
	// Field 是数据文件里的 JSON 键，大小写不敏感匹配（与 encoding/json 一样）。
	// 一行里同一列有几种大小写拼写时，取文档顺序里最后一个，即加载层解出的值，见 Rows。
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
	Rule   string // required / unique / min / enum / ref
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
//
// 一行里有几个键只差大小写（"level"、"Level"）时，只保留文档顺序里最后一个。
// 加载层用 encoding/json 把同一份载荷解进行结构体，几个键落到同一个字段时按文档
// 顺序最后一个生效，精确拼写并不优先；map 丢了顺序，所以在这里按原文定下来，规则
// 查到的就是类型化行里的值。之前 Lookup 先取精确键、否则遍历 map 取第一个变体：
// 精确键在前、变体在后时查错了值，没有精确键时取哪个随遍历顺序变（发版前审查观察）。
// 前提是行结构体里没有两个只差大小写的 JSON 名（那样 encoding/json 会先按精确名
// 分派，规则层不知道结构体的其他字段）。没有大小写变体的行原样返回，不多解析。
func Rows(payload []byte, object bool) ([]map[string]json.RawMessage, error) {
	if object {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(payload, &row); err != nil {
			return nil, err
		}
		if hasCaseVariants(row) {
			if err := keepLastCaseVariant(row, payload); err != nil {
				return nil, err
			}
		}
		return []map[string]json.RawMessage{row}, nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(payload, &rows); err != nil {
		return nil, err
	}
	var raw []json.RawMessage // 只在有行需要文档顺序时才解
	for i, row := range rows {
		if !hasCaseVariants(row) {
			continue
		}
		if raw == nil {
			if err := json.Unmarshal(payload, &raw); err != nil {
				return nil, err
			}
		}
		if err := keepLastCaseVariant(row, raw[i]); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// hasCaseVariants 报告一行里是否有两个键只差大小写。全是不含大写字母的 ASCII 键时
// 不可能有（常见的 snake_case），不分配。
func hasCaseVariants(row map[string]json.RawMessage) bool {
	plain := true
	for name := range row {
		for i := 0; i < len(name); i++ {
			if c := name[i]; c >= utf8.RuneSelf || ('A' <= c && c <= 'Z') {
				plain = false
				break
			}
		}
		if !plain {
			break
		}
	}
	if plain {
		return false
	}
	seen := make(map[string]struct{}, len(row))
	for name := range row {
		folded := foldName(name)
		if _, dup := seen[folded]; dup {
			return true
		}
		seen[folded] = struct{}{}
	}
	return false
}

// keepLastCaseVariant 按 object（这一行的原文）里键的顺序，在每组只差大小写的键里只留最后一个。
func keepLastCaseVariant(row map[string]json.RawMessage, object []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(object))
	if _, err := decoder.Token(); err != nil { // {
		return err
	}
	last := make(map[string]string, len(row))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return fmt.Errorf("object key is %v, not a string", token)
		}
		last[foldName(name)] = name
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	for name := range row {
		if last[foldName(name)] != name {
			delete(row, name)
		}
	}
	return nil
}

// foldName 把每个字符换成它大小写等价类里最小的那个：foldName(a) == foldName(b)
// 恰好是 strings.EqualFold(a, b)，与 encoding/json 匹配键时的折叠相同（含 K / ſ 这类
// 非 ASCII 等价字符）。
func foldName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		smallest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < smallest {
				smallest = f
			}
		}
		b.WriteRune(smallest)
	}
	return b.String()
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

// Lookup 在一行里找 field 列：精确键，否则大小写不敏感的键。Rows 解出的行每组大小写
// 变体只剩文档顺序里最后一个（encoding/json 解进结构体的那个），所以最多一个候选。
// 不是 Rows 解出的行若仍有几个变体，取字节序最小的键——结果确定，但文档顺序只有 Rows 知道。
func Lookup(row map[string]json.RawMessage, field string) (json.RawMessage, bool) {
	if value, ok := row[field]; ok {
		return value, true
	}
	var (
		chosen string
		value  json.RawMessage
		found  bool
	)
	for name, candidate := range row {
		if strings.EqualFold(name, field) && (!found || name < chosen) {
			chosen, value, found = name, candidate, true
		}
	}
	return value, found
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
