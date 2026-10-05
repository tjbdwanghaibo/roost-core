package app

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// 严格读取布尔与时长配置（RR-20261005-NC-190）。
//
// viper 的 GetBool / GetDuration 经 spf13/cast 宽松转换：转换失败返回零值而不报错，cast.ToDuration
// 还会给不带单位的数字补 "ns"。于是 YAML 里的 `enabled: on`（yaml.v3 按 YAML 1.2 读成字符串）读成
// false，`ttl: 15` 读成 15ns，两者都能通过只看转换结果的校验——单实例锁就这样被静默关掉。
// 这两个函数对“设置了但不是合法值”报错并点名键，未设置时返回零值，由调用方决定默认值。
// 框架里其余直接用 GetBool / GetDuration 的读取点见 docs/review/REVIEW-2026-10-05-n14.md 方向判断。

// ConfigBool 严格读取一个布尔配置。接受 YAML 布尔、strconv.ParseBool 认的字符串（true / false /
// True / FALSE / t / f / 1 / 0 等）与整数 0 / 1——这些是宽松读取下本来就得到正确结果的写法；
// `on` / `yes` / `off` / `no` 与拼写错误报错。未设置（或值为空）返回 false。
func ConfigBool(cfg *viper.Viper, key string) (bool, error) {
	if cfg == nil || !cfg.IsSet(key) {
		return false, nil
	}
	raw := cfg.Get(key)
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
		if n, ok := configInteger(raw); ok && (n == 0 || n == 1) {
			return n == 1, nil
		}
	}
	return false, fmt.Errorf("config: %s must be true or false, got %q", key, fmt.Sprint(raw))
}

// ConfigDuration 严格读取一个时长配置。接受 time.ParseDuration 认的字符串（15s、500ms、1m30s）、
// 代码里 Set 的 time.Duration，以及 0；不带单位的非零数字（YAML 数字或 "15" 这样的字符串）与解析
// 不了的值报错，不再当作纳秒。未设置（或值为空）返回 0。负数照样返回，由调用方判断。
func ConfigDuration(cfg *viper.Viper, key string) (time.Duration, error) {
	if cfg == nil || !cfg.IsSet(key) {
		return 0, nil
	}
	raw := cfg.Get(key)
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
		n, ok := configNumber(raw)
		if !ok {
			return 0, fmt.Errorf("config: %s = %v is not a duration (for example 15s or 500ms)", key, raw)
		}
		if n == 0 {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("config: %s = %v needs a unit (for example 15s or 500ms); a bare number would be read as nanoseconds", key, raw)
}

// configInteger 识别 YAML / viper 可能给出的整数类型。
func configInteger(raw any) (int64, bool) {
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
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	}
	return 0, false
}

// configNumber 识别整数与浮点数（YAML 的 1.5 解码为 float64）。
func configNumber(raw any) (float64, bool) {
	if n, ok := configInteger(raw); ok {
		return float64(n), true
	}
	switch n := raw.(type) {
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
