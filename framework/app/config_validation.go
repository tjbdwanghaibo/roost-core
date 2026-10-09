package app

import (
	"github.com/spf13/viper"
)

// ValidateServiceConfig 检查 App 自己的键：appConfig 的声明（日志、业务时钟偏移、停机预算、指标上限、单实例锁）与
// 它的跨键规则（server_type、sid、生产环境偏移为 0、单实例锁的三条时间关系）。
//
// 维护者决定 A4 ①（docs/feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md）：框架 Mod 的键不再登记在这里的清单里，
// 由各 Mod 的声明检查——App 启动时 CheckConfig 合并本服务全部 Mod 的声明，在任何 Mod Init 之前一次报全。
// 生产规则也跟着键的主人走：密钥（secret 声明）、ops 端点不绑公网（kit/ops）、Redis 地址（kit/redis 的 Redis 配置）。
func ValidateServiceConfig(cfg *viper.Viper) error {
	return CheckConfig(cfg)
}

// uniqueErrors 去掉文本相同的重复错误：两个 Mod 共用同一个键的声明时，写错的值会被各报一次。
func uniqueErrors(errs []error) []error {
	seen := make(map[string]bool, len(errs))
	out := errs[:0]
	for _, err := range errs {
		if err == nil || seen[err.Error()] {
			continue
		}
		seen[err.Error()] = true
		out = append(out, err)
	}
	return out
}
