package app

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// 单键的严格读取（RR-20261005-NC-190、维护者决定 A4 ②）。
//
// Mod 读配置用 LoadConfig 按声明一次读完（A4 ①）；这几个函数留给工具与测试临时读一个键。规则与声明读取相同：
// `on` / `yes` 不是布尔值、不带单位的时长、`8k` 这样的整数都报错并点名键，未设置时返回零值。
// 框架代码（app、kit）不用它们读配置（TestFrameworkModsReadConfigOnlyThroughDeclarations）。

// ConfigBool 严格读取一个布尔配置，规则见 configschema.ParseBool。
func ConfigBool(cfg *viper.Viper, key string) (bool, error) {
	return configschema.ParseBool(key, rawConfig(cfg, key))
}

// ConfigDuration 严格读取一个时长配置，规则见 configschema.ParseDuration。
func ConfigDuration(cfg *viper.Viper, key string) (time.Duration, error) {
	return configschema.ParseDuration(key, rawConfig(cfg, key))
}

// ConfigInt 严格读取一个整数配置，规则见 configschema.ParseInt。
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
	return configschema.ParseInt(key, rawConfig(cfg, key))
}

func rawConfig(cfg *viper.Viper, key string) any {
	if cfg == nil || !cfg.IsSet(key) {
		return nil
	}
	return cfg.Get(key)
}
