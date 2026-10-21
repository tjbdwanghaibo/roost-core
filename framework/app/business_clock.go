package app

import (
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
)

// ModBusinessClock 是业务时钟在 Registry 里的能力名（业务时钟与系统时钟的划分见
// docs/framework/guide/10-time.md）。NewRegistry 按 time.logic_offset 登记它。
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

// BusinessCalendar 返回业务日历：业务时区 time.business_timezone（缺省 UTC）与每日重置时刻
// time.business_day_start（缺省 00:00）。它挂在 Registry 的业务时钟上（clock.CalendarOf），日 / 周重置、
// “今天领过没有”按它算：
//
//	cal := app.BusinessCalendar(r)
//	today := cal.BusinessDay(app.BusinessClock(r).Now())
//
// 存储与协议里仍是绝对时刻（Unix 时间戳），“哪一天”只在这里算，不读 time.Local。
func BusinessCalendar(r *Registry) clock.Calendar {
	return clock.CalendarOf(BusinessClock(r))
}
