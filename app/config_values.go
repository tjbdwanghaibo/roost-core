package app

import (
	"errors"
	"fmt"
	"math"
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
// 这里的函数对“设置了但不是合法值”报错并点名键，未设置时返回零值，由调用方决定默认值。
//
// 维护者决定 A4（2026-10-05）：框架（app、kit）里的布尔、时长与整数配置一律经这里读取，不再直接用
// viper 的 GetBool / GetDuration / GetInt；ValidateServiceConfig 在任何 Mod Init 之前按同一规则把
// frameworkBoolKeys / frameworkDurationKeys / frameworkIntKeys 登记的键全部检查一遍。app 的
// TestFrameworkCodeDoesNotReadConfigLeniently 与 TestEvery*IsCheckedStrictly 扫描源码，新增宽松读取或漏登记
// 的键会让测试变红。方案见 docs/feature/REFACTOR-2026-10-05-strict-config-reads.md。

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

// ConfigInt 严格读取一个整数配置。接受 YAML 整数、没有小数部分的浮点数（YAML 的 1e3）与
// strconv.ParseInt 认的十进制字符串（环境变量覆盖都是字符串）；`8k`、`1.5`、`10s`、布尔值与解析不了的
// 值报错——宽松读取会把它们读成 0（取默认）或截断。未设置（或值为空）返回 0。
func ConfigInt(cfg *viper.Viper, key string) (int, error) {
	value, err := ConfigInt64(cfg, key)
	if err != nil {
		return 0, err
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("config: %s = %d does not fit in an int", key, value)
	}
	return int(value), nil
}

// ConfigInt64 与 ConfigInt 相同，返回 int64。
func ConfigInt64(cfg *viper.Viper, key string) (int64, error) {
	if cfg == nil || !cfg.IsSet(key) {
		return 0, nil
	}
	raw := cfg.Get(key)
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
		if n, ok := configInteger(raw); ok {
			return n, nil
		}
		if f, ok := configNumber(raw); ok && f == math.Trunc(f) && math.Abs(f) < 1<<63 {
			return int64(f), nil
		}
	}
	return 0, fmt.Errorf("config: %s must be a whole number, got %q", key, fmt.Sprint(raw))
}

// ConfigReader 按顺序严格读取一组配置，把每个键的类型错误攒起来，最后由 Err 一次报出——Mod 的 Init
// 一次读几十个键，逐个 `if err != nil { return err }` 会把读取淹没在错误分支里，而运维也希望一次看到
// 全部写错的键。读取失败的键返回零值，调用方照常按“≤0 取默认”处理，只要在用这些值做语义检查或
// 返回之前检查 Err。
type ConfigReader struct {
	cfg  *viper.Viper
	errs []error
}

// NewConfigReader 返回读取 cfg 的 ConfigReader；cfg 为 nil 时所有读取返回零值。
func NewConfigReader(cfg *viper.Viper) *ConfigReader { return &ConfigReader{cfg: cfg} }

// Bool 见 ConfigBool。
func (r *ConfigReader) Bool(key string) bool {
	value, err := ConfigBool(r.cfg, key)
	r.note(err)
	return value
}

// Duration 见 ConfigDuration。
func (r *ConfigReader) Duration(key string) time.Duration {
	value, err := ConfigDuration(r.cfg, key)
	r.note(err)
	return value
}

// Int 见 ConfigInt。
func (r *ConfigReader) Int(key string) int {
	value, err := ConfigInt(r.cfg, key)
	r.note(err)
	return value
}

// Int64 见 ConfigInt64。
func (r *ConfigReader) Int64(key string) int64 {
	value, err := ConfigInt64(r.cfg, key)
	r.note(err)
	return value
}

// Err 返回到目前为止全部读取错误（errors.Join），没有错误时为 nil。
func (r *ConfigReader) Err() error { return errors.Join(r.errs...) }

func (r *ConfigReader) note(err error) {
	if err != nil {
		r.errs = append(r.errs, err)
	}
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
