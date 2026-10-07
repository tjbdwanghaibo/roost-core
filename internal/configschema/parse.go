package configschema

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 严格类型解析（RR-20261005-NC-190、维护者决定 A4）。
//
// viper 的 GetBool / GetDuration / GetInt 经 spf13/cast 宽松转换：转换失败返回零值而不报错，cast.ToDuration
// 还会给不带单位的数字补 "ns"。于是 YAML 里的 `enabled: on`（yaml.v3 按 YAML 1.2 读成字符串）读成 false，
// `ttl: 15` 读成 15ns，`workers: 8k` 读成 0 取默认。这里对“写了但不是合法值”报错并点名键；错误文本与
// A4 ② 的 app.ConfigBool / ConfigDuration / ConfigInt 相同（它们现在调用这里）。

// ParseBool 接受 YAML 布尔、strconv.ParseBool 认的字符串（true / false / True / FALSE / t / f / 1 / 0 等）与整数 0 / 1；
// `on` / `yes` / `off` / `no` 与拼写错误报错。nil 返回 false。
func ParseBool(key string, raw any) (bool, error) {
	switch value := raw.(type) {
	case nil:
		return false, nil
	case bool:
		return value, nil
	case string:
		if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
			return parsed, nil
		}
	default:
		if n, ok := integer(raw); ok && (n == 0 || n == 1) {
			return n == 1, nil
		}
	}
	return false, fmt.Errorf("config: %s must be true or false, got %q", key, fmt.Sprint(raw))
}

// ParseDuration 接受 time.ParseDuration 认的字符串（15s、500ms、1m30s）、代码里 Set 的 time.Duration，以及 0；
// 不带单位的非零数字与解析不了的值报错，不再当作纳秒。nil 返回 0；负数照样返回，由范围检查判断。
func ParseDuration(key string, raw any) (time.Duration, error) {
	switch value := raw.(type) {
	case nil:
		return 0, nil
	case time.Duration:
		return value, nil
	case string:
		text := strings.TrimSpace(value)
		if text == "" || text == "0" {
			return 0, nil
		}
		if parsed, err := time.ParseDuration(text); err == nil {
			return parsed, nil
		}
		if _, err := strconv.ParseFloat(text, 64); err != nil {
			return 0, fmt.Errorf("config: %s = %q is not a duration (for example 15s or 500ms)", key, text)
		}
	default:
		n, ok := number(raw)
		if !ok {
			return 0, fmt.Errorf("config: %s = %v is not a duration (for example 15s or 500ms)", key, raw)
		}
		if n == 0 {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("config: %s = %v needs a unit (for example 15s or 500ms); a bare number would be read as nanoseconds", key, raw)
}

// ParseInt 接受 YAML 整数、没有小数部分的浮点数（YAML 的 1e3）与十进制字符串（文本输入）；
// `8k`、`1.5`、`10s`、布尔值与解析不了的值报错。nil 或空串返回 0。
func ParseInt(key string, raw any) (int64, error) {
	switch value := raw.(type) {
	case nil:
		return 0, nil
	case bool:
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return 0, nil
		}
		if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
			return parsed, nil
		}
	default:
		if n, ok := integer(raw); ok {
			return n, nil
		}
		if f, ok := number(raw); ok && f == math.Trunc(f) && math.Abs(f) < 1<<63 {
			return int64(f), nil
		}
	}
	return 0, fmt.Errorf("config: %s must be a whole number, got %q", key, fmt.Sprint(raw))
}

// ParseFloat 接受数字与十进制字符串。
func ParseFloat(key string, raw any) (float64, error) {
	switch value := raw.(type) {
	case nil:
		return 0, nil
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return 0, nil
		}
		if parsed, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			return parsed, nil
		}
	default:
		if n, ok := number(raw); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n, nil
		}
	}
	return 0, fmt.Errorf("config: %s must be a number, got %q", key, fmt.Sprint(raw))
}

// ParseString 接受标量（YAML 把 `prefix: 123` 读成整数，这里还原成 "123"），去掉两端空白（配置里首尾的空白
// 一律是笔误：键前缀、库名、地址都不该带）；映射与列表报错。nil 返回空串。
func ParseString(key string, raw any) (string, error) {
	if raw == nil {
		return "", nil
	}
	// 代码里 Set 的值可能是具名字符串类型（app.ServiceName）或 time.Duration，按底层种类还原。
	switch value := reflect.ValueOf(raw); value.Kind() {
	case reflect.String:
		return strings.TrimSpace(value.String()), nil
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return fmt.Sprint(raw), nil
	}
	return "", fmt.Errorf("config: %s must be a string, got %v", key, raw)
}

// ParseStrings 接受 YAML 列表与逗号分隔的字符串（文本输入可写成后者），去掉每项两端空白、丢弃空项。
func ParseStrings(key string, raw any) ([]string, error) {
	var items []string
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		items = strings.Split(value, ",")
	case []string:
		items = value
	case []any:
		for _, item := range value {
			text, err := ParseString(key, item)
			if err != nil {
				return nil, fmt.Errorf("config: %s must be a list of strings, got %v", key, raw)
			}
			items = append(items, text)
		}
	default:
		return nil, fmt.Errorf("config: %s must be a list of strings, got %v", key, raw)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// parseRaw 按键的 Kind 解析配置里的值。返回值的类型：bool、int64、float64、time.Duration、string、[]string。
func parseRaw(k Key, raw any) (any, error) {
	switch k.Kind {
	case KindBool:
		return ParseBool(k.Name, raw)
	case KindInt:
		return ParseInt(k.Name, raw)
	case KindFloat:
		return ParseFloat(k.Name, raw)
	case KindDuration:
		return ParseDuration(k.Name, raw)
	case KindString:
		text, err := ParseString(k.Name, raw)
		if err == nil && len(k.Enum) > 0 {
			text = strings.ToLower(strings.TrimSpace(text))
		}
		return text, err
	case KindStrings:
		return ParseStrings(k.Name, raw)
	}
	return nil, fmt.Errorf("config: %s has no value kind", k.Name)
}

// parseValue 解析声明里的文本（default / example / min / max）。
func parseValue(k Key, text string) (any, error) {
	if k.Kind == KindStrings {
		trimmed := strings.TrimSpace(text)
		if trimmed == "[]" {
			return []string(nil), nil
		}
		return ParseStrings(k.Name, strings.Trim(trimmed, "[]"))
	}
	if k.Kind == KindMap {
		return nil, nil
	}
	return parseRaw(k, text)
}

// isEmpty 报告值是否算“没写”（必填检查用）。
func isEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []string:
		return len(v) == 0
	}
	return false
}

// checkValue 检查范围与枚举。min 是最小正数（1、1ns）时报 “must be positive”，min 为 0 时报 “must not be negative”，
// 与 A4 ② 之前各 Mod 手写的文本一致。
func (k Key) checkValue(value any) error {
	if len(k.Enum) > 0 {
		text, _ := value.(string)
		if text != "" && !slices.Contains(k.Enum, text) {
			return fmt.Errorf("config: %s must be one of %s, got %q", k.Name, strings.Join(k.Enum, ", "), text)
		}
	}
	if k.Min == "" && k.Max == "" {
		return nil
	}
	n, ok := comparable(value)
	if !ok {
		return nil
	}
	if k.Min != "" {
		low, _ := parseValue(k, k.Min)
		lowNumber, _ := comparable(low)
		if n < lowNumber {
			switch {
			case lowNumber == 0:
				return fmt.Errorf("config: %s must not be negative, got %v", k.Name, value)
			case lowNumber == 1 && k.Kind != KindFloat:
				return fmt.Errorf("config: %s must be positive, got %v", k.Name, value)
			}
			return fmt.Errorf("config: %s must be at least %s, got %v", k.Name, k.Min, value)
		}
	}
	if k.Max != "" {
		high, _ := parseValue(k, k.Max)
		if highNumber, _ := comparable(high); n > highNumber {
			return fmt.Errorf("config: %s must be at most %s, got %v", k.Name, k.Max, value)
		}
	}
	return nil
}

func comparable(value any) (float64, bool) {
	switch v := value.(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case time.Duration:
		return float64(v), true
	}
	return 0, false
}

// fits 检查整数是否放得进字段的 Go 类型（int32 的 sid、uint32 的字节上限）。
func fits(key string, typ reflect.Type, value any) error {
	n, ok := value.(int64)
	if !ok {
		return nil
	}
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if reflect.Zero(typ).OverflowInt(n) {
			return fmt.Errorf("config: %s = %d does not fit in an %s", key, n, typ)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n < 0 {
			return fmt.Errorf("config: %s must not be negative, got %d", key, n)
		}
		if reflect.Zero(typ).OverflowUint(uint64(n)) {
			return fmt.Errorf("config: %s = %d does not fit in a %s", key, n, typ)
		}
	}
	return nil
}

func integer(raw any) (int64, bool) {
	switch n := raw.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		if uint64(n) > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}

func number(raw any) (float64, bool) {
	if n, ok := integer(raw); ok {
		return float64(n), true
	}
	switch n := raw.(type) {
	case uint64:
		return float64(n), true
	case uint:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
