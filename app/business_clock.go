package app

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/clock"
)

// ModBusinessClock 是业务时钟在 Registry 里的能力名（维护者决定 D-L3，
// docs/feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md）。NewRegistry 按 time.logic_offset 登记它。
const ModBusinessClock ModName = "clock.business"

// logicOffsetKey 是业务时钟偏移唯一的配置来源。所有进程读同一份：参与同一活动组的 game 与协调器
// 偏移不同，窗口 id 与截止就会错开。
const logicOffsetKey = "time.logic_offset"

// BusinessClock 返回 Registry 里的业务时钟：真实时间加 time.logic_offset。活动窗口、World 定时器、
// 冷却、邮件 / 道具业务过期、赛季、排行周期等业务时间都读它；租约、锁、超时、重试、存储 TTL、
// 日志与 WAL 时间戳是系统时钟，直接用 time 包。
//
// 业务服务的 Config.Now 由 Mod 注入 BusinessClock(r).Now；测试替换时在 cfg 里写偏移，或给服务的
// Config.Now 直接注入。r 为 nil 或没有登记时退回进程级业务时钟（clock.Process）。
func BusinessClock(r *Registry) clock.Business {
	if business, ok := Lookup[clock.Business](r, ModBusinessClock); ok && business != nil {
		return business
	}
	return clock.Process()
}

// configuredLogicOffset 读 time.logic_offset。类型错误由 ValidateServiceConfig 报出；这里读不出就当 0，
// 不在 NewRegistry 里重复报错。
func configuredLogicOffset(cfg *viper.Viper) time.Duration {
	offset, err := ConfigDuration(cfg, logicOffsetKey)
	if err != nil {
		return 0
	}
	return offset
}

// validateProductionLogicOffset：生产环境偏移必须为 0（D-L3）。偏移只给测试环境前拨业务时间用，
// 生产进程带着偏移启动，活动、邮件、副本截止都按错的时间走。
func validateProductionLogicOffset(errs *[]error, cfg *viper.Viper) {
	offset, err := ConfigDuration(cfg, logicOffsetKey)
	if err != nil || offset == 0 {
		return // 类型错误由通用的时长检查报出
	}
	*errs = append(*errs, fmt.Errorf("config: production requires %s = 0, got %s; the offset is for moving business time in test environments", logicOffsetKey, offset))
}
