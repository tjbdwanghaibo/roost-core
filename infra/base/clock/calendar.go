package clock

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// 业务日（DESIGN-TODO D18，维护者 2026-10-20 决定 A）。
//
// 存储与协议里的时间一律是绝对时刻（Unix 时间戳 / UTC），“哪一天”只在业务时区里算：业务时区与每日重置时刻
// 是部署配置（time.business_timezone，缺省 UTC；time.business_day_start，缺省 00:00），App 启动时读一次，
// 不取机器的 time.Local 或 TZ 环境变量——同一份代码在开发机（本地时区）和容器（UTC）上跨天时刻不同。
//
// 每日重置的推荐写法：DAO 里存上次重置的业务日（Day），登录 / 首次操作时 BusinessDay(now) 不同就惰性重置；
// 需要准点推送（零点发奖、活动开门）时用按实体的持久定时器，期限取 NextDayStart(now)。周重置同理，由业务按 Day 推。

// Day 是一个业务日：业务时区下、按每日重置时刻划分的日历日期，写成十进制 YYYYMMDD（2026-10-20 是 20261020）。
// 可以直接存进 DAO、按大小比较先后；零值表示“还没有过”。只表示日期，不做日期运算（相差几天用 DayStart 的时刻算）。
type Day int32

// Date 拆出年、月、日。
func (d Day) Date() (year int, month time.Month, day int) {
	return int(d) / 10000, time.Month(int(d) / 100 % 100), int(d) % 100
}

// String 是 2026-10-20 形式。
func (d Day) String() string {
	year, month, day := d.Date()
	return fmt.Sprintf("%04d-%02d-%02d", year, int(month), day)
}

// next 是日历上的下一天（测试核对用）。
func (d Day) next() Day {
	year, month, day := d.Date()
	return dayOf(time.Date(year, month, day+1, 12, 0, 0, 0, time.UTC).Date())
}

func dayOf(year int, month time.Month, day int) Day {
	return Day(year*10000 + int(month)*100 + day)
}

// Calendar 是业务日历：业务时区 + 每日重置时刻（当地墙上时间）。业务日从当地的重置时刻开始，到下一个当地重置时刻
// 结束；夏令时切换那天是 23 或 25 小时，不按固定 24 小时推。零值是 UTC、00:00 重置。值类型，可以随意复制。
type Calendar struct {
	loc      *time.Location // nil 表示 UTC
	dayStart time.Duration  // 当地 0 点之后多久重置，[0, 24h)，分钟精度
}

// NewCalendar 用已加载的时区与重置时刻（当地 0 点之后多久，[0, 24h)，整分钟）构造日历。loc 为 nil 表示 UTC。
func NewCalendar(loc *time.Location, dayStart time.Duration) (Calendar, error) {
	if dayStart < 0 || dayStart >= 24*time.Hour || dayStart%time.Minute != 0 {
		return Calendar{}, fmt.Errorf("clock: business day start %v must be whole minutes in [0, 24h)", dayStart)
	}
	if loc == time.Local {
		return Calendar{}, errors.New("clock: the business time zone cannot be time.Local: it depends on the machine (TZ, /etc/localtime); name an IANA zone such as Asia/Shanghai")
	}
	return Calendar{loc: loc, dayStart: dayStart}, nil
}

// ParseCalendar 按配置写法构造日历：timezone 见 LoadTimezone，dayStart 见 ParseDayStart。两处错误一起报。
func ParseCalendar(timezone, dayStart string) (Calendar, error) {
	loc, zoneErr := LoadTimezone(timezone)
	offset, startErr := ParseDayStart(dayStart)
	if err := errors.Join(zoneErr, startErr); err != nil {
		return Calendar{}, err
	}
	return NewCalendar(loc, offset)
}

// LoadTimezone 加载业务时区：IANA 时区名（UTC、Asia/Shanghai、America/New_York）。Local（取决于机器环境）、
// 空名与加载不了的名字都拒绝。
//
// 加载时区需要时区数据库：生成工程的 main 导入 time/tzdata（内嵌约 450 KB），没有 zoneinfo 的运行镜像
// （distroless static）也能加载；手写的 main 在这类镜像里要自己导入，否则这里报 unknown time zone。
// 注意 macOS 的文件系统不区分大小写，asia/shanghai 在开发机上能从系统 zoneinfo 加载、在 Linux 上不能：照 IANA 原样写。
func LoadTimezone(name string) (*time.Location, error) {
	switch name {
	case "":
		return nil, errors.New(`business time zone is empty; name an IANA time zone, e.g. UTC or Asia/Shanghai`)
	case "Local":
		return nil, errors.New(`business time zone "Local" depends on the machine (TZ, /etc/localtime); name an IANA time zone, e.g. UTC or Asia/Shanghai`)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("business time zone %q is not a time zone this process can load (an IANA name such as Asia/Shanghai; "+
			`a main without import _ "time/tzdata" also needs zoneinfo on the host): %w`, name, err)
	}
	return loc, nil
}

// ParseDayStart 解析每日重置时刻 HH:MM（00:00～23:59，业务时区的墙上时间），返回当地 0 点之后多久。
// 只认两位小时、两位分钟：05:00 与 5:00 看起来一样，但只有一种写法，配置里不会出现两种。
func ParseDayStart(value string) (time.Duration, error) {
	bad := fmt.Errorf("business day start %q must be HH:MM between 00:00 and 23:59 (local wall time in the business time zone)", value)
	if len(value) != 5 || value[2] != ':' {
		return 0, bad
	}
	digit := func(i int) int {
		if value[i] < '0' || value[i] > '9' {
			return -100
		}
		return int(value[i] - '0')
	}
	hour, minute := digit(0)*10+digit(1), digit(3)*10+digit(4)
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, bad
	}
	return time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute, nil
}

// Location 是业务时区。
func (c Calendar) Location() *time.Location {
	if c.loc == nil {
		return time.UTC
	}
	return c.loc
}

// DayStartOffset 是每日重置时刻：当地 0 点之后多久。
func (c Calendar) DayStartOffset() time.Duration { return c.dayStart }

// String 是“时区 HH:MM”，启动日志与错误信息用。
func (c Calendar) String() string {
	return fmt.Sprintf("%s %s", c.Location(), c.dayStartText())
}

func (c Calendar) dayStartText() string {
	return fmt.Sprintf("%02d:%02d", int(c.dayStart/time.Hour), int(c.dayStart%time.Hour/time.Minute))
}

// startOf 是日历日期 (year, month, day) 那个业务日的起点。day 可以越界，time.Date 会规范化。
// 重置时刻落在夏令时跳过的一小时里时，time.Date 取哪一侧不保证，但对日期单调，bounds 的不变量不受影响。
func (c Calendar) startOf(year int, month time.Month, day int) time.Time {
	minutes := int(c.dayStart / time.Minute)
	return time.Date(year, month, day, minutes/60, minutes%60, 0, 0, c.Location())
}

// bounds 返回 t 所在业务日的日期与 [起点, 下一个起点)。先取 t 的当地日期，再按起点前后最多挪一天：
// 重置时刻之前的时间属前一天；夏令时让起点前后移一两个小时时也收敛到满足 start ≤ t < next 的那一天。
func (c Calendar) bounds(t time.Time) (Day, time.Time, time.Time) {
	year, month, day := t.In(c.Location()).Date()
	start := c.startOf(year, month, day)
	for t.Before(start) {
		day--
		start = c.startOf(year, month, day)
	}
	next := c.startOf(year, month, day+1)
	for !t.Before(next) {
		day++
		start, next = next, c.startOf(year, month, day+1)
	}
	return dayOf(time.Date(year, month, day, 12, 0, 0, 0, time.UTC).Date()), start, next
}

// BusinessDay 是 t 所在的业务日。
func (c Calendar) BusinessDay(t time.Time) Day {
	day, _, _ := c.bounds(t)
	return day
}

// DayStart 是 t 所在业务日的起点（业务时区里的时刻），不晚于 t。
func (c Calendar) DayStart(t time.Time) time.Time {
	_, start, _ := c.bounds(t)
	return start
}

// NextDayStart 是 t 之后下一个业务日的起点，晚于 t。准点重置的定时器期限取它。
func (c Calendar) NextDayStart(t time.Time) time.Time {
	_, _, next := c.bounds(t)
	return next
}

// processCalendar 是进程级业务日历，App 启动时按配置 SetCalendar 一次；没设是 UTC 00:00。
var processCalendar atomic.Pointer[Calendar]

// SetCalendar 设置进程级业务日历。由 App 在启动时按 time.business_timezone / time.business_day_start 调用一次；
// 运行期不许调用，测试可以调用并在结束时恢复。
func SetCalendar(c Calendar) { processCalendar.Store(&c) }

// ProcessCalendar 返回进程级业务日历。
func ProcessCalendar() Calendar {
	if c := processCalendar.Load(); c != nil {
		return *c
	}
	return Calendar{}
}

// BusinessDay / DayStart / NextDayStart 按进程级业务日历计算，给没有注入日历的业务代码与框架库用；
// 有 Registry 的代码用 app.BusinessCalendar(registry)。
func BusinessDay(t time.Time) Day { return ProcessCalendar().BusinessDay(t) }

// DayStart 见 Calendar.DayStart。
func DayStart(t time.Time) time.Time { return ProcessCalendar().DayStart(t) }

// NextDayStart 见 Calendar.NextDayStart。
func NextDayStart(t time.Time) time.Time { return ProcessCalendar().NextDayStart(t) }

// calendarCarrier 由带日历的业务时钟实现（NewBusinessIn、Process）。
type calendarCarrier interface {
	Calendar() Calendar
}

// CalendarOf 返回业务时钟带着的日历；时钟没有自己的日历（测试替换的 BusinessFunc、nil）时取进程级日历。
func CalendarOf(b Business) Calendar {
	if carrier, ok := b.(calendarCarrier); ok {
		return carrier.Calendar()
	}
	return ProcessCalendar()
}
