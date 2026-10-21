package clock

import (
	"strings"
	"testing"
	"time"
)

// 业务日（DESIGN-TODO D18）：业务时区 + 每日重置时刻把时间轴切成业务日。
// 用例覆盖缺省 UTC、正 / 负偏移时区、重置时刻落在当地凌晨（跨日归前一天）、夏令时切换日（23h / 25h、
// 重置时刻落在不存在或重复的那一小时），并在一整年上逐点核对三条不变量。

func mustCalendar(t *testing.T, timezone, dayStart string) Calendar {
	t.Helper()
	cal, err := ParseCalendar(timezone, dayStart)
	if err != nil {
		t.Fatalf("ParseCalendar(%q, %q): %v", timezone, dayStart, err)
	}
	return cal
}

func utc(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

func TestZeroCalendarIsUTCMidnight(t *testing.T) {
	var cal Calendar
	if cal.Location() != time.UTC || cal.DayStartOffset() != 0 {
		t.Fatalf("zero calendar = %s %v, want UTC 00:00", cal.Location(), cal.DayStartOffset())
	}
	at := utc("2026-10-20T23:59:59Z")
	if got := cal.BusinessDay(at); got != 20261020 {
		t.Fatalf("BusinessDay = %d, want 20261020", got)
	}
	if got := cal.DayStart(at); !got.Equal(utc("2026-10-20T00:00:00Z")) {
		t.Fatalf("DayStart = %v", got)
	}
	if got := cal.NextDayStart(at); !got.Equal(utc("2026-10-21T00:00:00Z")) {
		t.Fatalf("NextDayStart = %v", got)
	}
	if got := cal.BusinessDay(utc("2026-10-21T00:00:00Z")); got != 20261021 {
		t.Fatalf("the day start belongs to the new day: BusinessDay = %d, want 20261021", got)
	}
	if got := cal.String(); got != "UTC 00:00" {
		t.Fatalf("String = %q", got)
	}
}

// 东八区、每天 05:00 重置：当地 00:00～05:00 仍属前一个业务日，跨月、跨年同理。
func TestPositiveOffsetWithAMorningReset(t *testing.T) {
	cal := mustCalendar(t, "Asia/Shanghai", "05:00")
	cases := []struct {
		at        string
		day       Day
		start     string
		nextStart string
	}{
		// 当地 2026-10-21 04:30：还没到 05:00，属 10-20。
		{"2026-10-20T20:30:00Z", 20261020, "2026-10-19T21:00:00Z", "2026-10-20T21:00:00Z"},
		// 当地 2026-10-21 05:00 整：新的一天从这一刻开始。
		{"2026-10-20T21:00:00Z", 20261021, "2026-10-20T21:00:00Z", "2026-10-21T21:00:00Z"},
		// 当地 2027-01-01 03:00：跨年但没到重置时刻，仍是 2026-12-31。
		{"2026-12-31T19:00:00Z", 20261231, "2026-12-30T21:00:00Z", "2026-12-31T21:00:00Z"},
		// 当地 2026-11-01 04:59：跨月前一刻。
		{"2026-10-31T20:59:00Z", 20261031, "2026-10-30T21:00:00Z", "2026-10-31T21:00:00Z"},
	}
	for _, c := range cases {
		at := utc(c.at)
		if got := cal.BusinessDay(at); got != c.day {
			t.Errorf("%s: BusinessDay = %d, want %d", c.at, got, c.day)
		}
		if got := cal.DayStart(at); !got.Equal(utc(c.start)) {
			t.Errorf("%s: DayStart = %v, want %s", c.at, got.UTC(), c.start)
		}
		if got := cal.NextDayStart(at); !got.Equal(utc(c.nextStart)) {
			t.Errorf("%s: NextDayStart = %v, want %s", c.at, got.UTC(), c.nextStart)
		}
		if got := cal.DayStart(at).Location(); got.String() != "Asia/Shanghai" {
			t.Errorf("%s: DayStart is in %s, want the business time zone", c.at, got)
		}
	}
	if got := cal.String(); got != "Asia/Shanghai 05:00" {
		t.Fatalf("String = %q", got)
	}
}

// 负偏移（UTC−10，无夏令时）：UTC 已经是第二天，业务日仍是前一天。
func TestNegativeOffset(t *testing.T) {
	cal := mustCalendar(t, "Pacific/Honolulu", "00:00")
	at := utc("2026-10-21T05:00:00Z") // 当地 2026-10-20 19:00
	if got := cal.BusinessDay(at); got != 20261020 {
		t.Fatalf("BusinessDay = %d, want 20261020 (UTC is already the 21st)", got)
	}
	if got := cal.DayStart(at); !got.Equal(utc("2026-10-20T10:00:00Z")) {
		t.Fatalf("DayStart = %v", got.UTC())
	}
	if got := cal.NextDayStart(at); !got.Equal(utc("2026-10-21T10:00:00Z")) {
		t.Fatalf("NextDayStart = %v", got.UTC())
	}
}

// America/New_York 2026-03-08 02:00 EST → 03:00 EDT（这一天 23 小时），2026-11-01 02:00 EDT → 01:00 EST（25 小时）。
// 重置时刻是墙上时间：业务日从当地 00:00 开始，不按固定 24 小时推。
func TestDaylightSavingDaysAreShortAndLong(t *testing.T) {
	cal := mustCalendar(t, "America/New_York", "00:00")
	spring := utc("2026-03-08T16:00:00Z") // 当地 12:00 EDT
	if got := cal.BusinessDay(spring); got != 20260308 {
		t.Fatalf("spring BusinessDay = %d", got)
	}
	start, next := cal.DayStart(spring), cal.NextDayStart(spring)
	if !start.Equal(utc("2026-03-08T05:00:00Z")) || !next.Equal(utc("2026-03-09T04:00:00Z")) {
		t.Fatalf("spring day = [%v, %v), want [05:00Z, next 04:00Z)", start.UTC(), next.UTC())
	}
	if got := next.Sub(start); got != 23*time.Hour {
		t.Fatalf("spring-forward day lasts %v, want 23h", got)
	}
	fall := utc("2026-11-01T16:00:00Z") // 当地 11:00 EST
	start, next = cal.DayStart(fall), cal.NextDayStart(fall)
	if got := next.Sub(start); got != 25*time.Hour {
		t.Fatalf("fall-back day [%v, %v) lasts %v, want 25h", start.UTC(), next.UTC(), got)
	}
	// 回拨的那一小时出现两次（当地 01:00～02:00），两次都属 11-01，不会切出第二个业务日。
	for _, at := range []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"} {
		if got := cal.BusinessDay(utc(at)); got != 20261101 {
			t.Fatalf("%s (01:30 local, %s): BusinessDay = %d", at, utc(at).In(cal.Location()).Format("MST"), got)
		}
	}
}

// 重置时刻落在不存在的一小时（02:30，春季被跳过）或重复的一小时（01:30，秋季出现两次）：
// 那一天仍然只有一个起点，起点单调、不变量成立。Go 对不存在的墙上时间的取法不保证，这里只断言不变量与日长范围。
func TestResetInsideTheDaylightSavingGapOrOverlap(t *testing.T) {
	for _, dayStart := range []string{"02:30", "01:30"} {
		cal := mustCalendar(t, "America/New_York", dayStart)
		for _, at := range []string{"2026-03-08T12:00:00Z", "2026-03-08T18:00:00Z", "2026-11-01T12:00:00Z", "2026-11-01T18:00:00Z"} {
			moment := utc(at)
			start, next := cal.DayStart(moment), cal.NextDayStart(moment)
			if moment.Before(start) || !moment.Before(next) {
				t.Fatalf("%s reset %s: %v not in [%v, %v)", at, dayStart, moment, start.UTC(), next.UTC())
			}
			if length := next.Sub(start); length < 22*time.Hour || length > 26*time.Hour {
				t.Fatalf("%s reset %s: business day lasts %v", at, dayStart, length)
			}
		}
	}
}

// 一整年每 17 分钟取一点，在多种日历上核对：
//  1. DayStart(t) ≤ t < NextDayStart(t)；
//  2. 起点属于同一个业务日，下一个起点属于日历上的下一天；
//  3. 起点是自己那一天的起点（DayStart(DayStart(t)) == DayStart(t)）。
func TestInvariantsHoldThroughAWholeYear(t *testing.T) {
	calendars := []Calendar{
		{},
		mustCalendar(t, "Asia/Shanghai", "05:00"),
		mustCalendar(t, "America/New_York", "00:00"),
		mustCalendar(t, "America/New_York", "02:30"),
		mustCalendar(t, "Europe/London", "01:30"),
		mustCalendar(t, "Australia/Lord_Howe", "02:00"), // 夏令时只差 30 分钟
		mustCalendar(t, "Asia/Kathmandu", "23:59"),      // +05:45
		mustCalendar(t, "Pacific/Honolulu", "12:00"),
	}
	from, to := utc("2026-01-01T00:00:00Z"), utc("2027-01-02T00:00:00Z")
	for _, cal := range calendars {
		for at := from; at.Before(to); at = at.Add(17 * time.Minute) {
			day, start, next := cal.BusinessDay(at), cal.DayStart(at), cal.NextDayStart(at)
			if at.Before(start) || !at.Before(next) {
				t.Fatalf("%s: %v not in [%v, %v)", cal, at, start, next)
			}
			if got := cal.BusinessDay(start); got != day {
				t.Fatalf("%s: start %v is day %d, %v is day %d", cal, start, got, at, day)
			}
			if got, want := cal.BusinessDay(next), day.next(); got != want {
				t.Fatalf("%s: next start %v is day %d, want %d", cal, next, got, want)
			}
			if got := cal.DayStart(start); !got.Equal(start) {
				t.Fatalf("%s: DayStart(%v) = %v", cal, start, got)
			}
		}
	}
}

func TestDayFormatsAndSplitsItsDate(t *testing.T) {
	d := Day(20260305)
	if got := d.String(); got != "2026-03-05" {
		t.Fatalf("String = %q", got)
	}
	if y, m, dd := d.Date(); y != 2026 || m != time.March || dd != 5 {
		t.Fatalf("Date = %d %v %d", y, m, dd)
	}
	if got := Day(0).String(); got != "0000-00-00" {
		t.Fatalf("zero Day String = %q", got)
	}
}

// 配置值在解析时拒绝：时区必须是可加载的 IANA 名（Local 取决于机器环境，拒绝）；重置时刻必须是 HH:MM。
func TestParseCalendarRejectsBadValues(t *testing.T) {
	for _, bad := range []struct{ timezone, dayStart, want string }{
		{"Mars/Olympus_Mons", "00:00", "time zone"},
		{"Local", "00:00", "Local"},
		{"", "00:00", "time zone"},
		{"UTC+8", "00:00", "time zone"},
		{"UTC", "24:00", "HH:MM"},
		{"UTC", "5:00", "HH:MM"},
		{"UTC", "05:60", "HH:MM"},
		{"UTC", "05:00:00", "HH:MM"},
		{"UTC", "", "HH:MM"},
	} {
		if _, err := ParseCalendar(bad.timezone, bad.dayStart); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("ParseCalendar(%q, %q) err = %v, want one mentioning %q", bad.timezone, bad.dayStart, err, bad.want)
		}
	}
	cal := mustCalendar(t, "Etc/UTC", "23:59")
	if cal.DayStartOffset() != 23*time.Hour+59*time.Minute {
		t.Fatalf("DayStartOffset = %v", cal.DayStartOffset())
	}
}

// 进程级日历由 App 启动时设一次；业务时钟对象带着自己的日历，测试替换的 BusinessFunc 退回进程级日历。
func TestBusinessClocksCarryTheirCalendar(t *testing.T) {
	saved := ProcessCalendar()
	t.Cleanup(func() { SetCalendar(saved) })
	shanghai := mustCalendar(t, "Asia/Shanghai", "05:00")
	SetCalendar(shanghai)
	at := utc("2026-10-20T20:30:00Z")
	if got := BusinessDay(at); got != 20261020 {
		t.Fatalf("process BusinessDay = %d", got)
	}
	if got := NextDayStart(at); !got.Equal(utc("2026-10-20T21:00:00Z")) {
		t.Fatalf("process NextDayStart = %v", got.UTC())
	}
	if got := DayStart(at); !got.Equal(utc("2026-10-19T21:00:00Z")) {
		t.Fatalf("process DayStart = %v", got.UTC())
	}
	if got := CalendarOf(Process()); got != shanghai {
		t.Fatalf("CalendarOf(Process()) = %s", got)
	}
	york := mustCalendar(t, "America/New_York", "00:00")
	if got := CalendarOf(NewBusinessIn(time.Hour, york)); got != york {
		t.Fatalf("CalendarOf(NewBusinessIn) = %s", got)
	}
	if got := CalendarOf(NewBusiness(0)); got != shanghai {
		t.Fatalf("CalendarOf(NewBusiness) = %s, want the process calendar", got)
	}
	if got := CalendarOf(BusinessFunc(time.Now)); got != shanghai {
		t.Fatalf("CalendarOf(BusinessFunc) = %s, want the process calendar", got)
	}
	if got := CalendarOf(nil); got != shanghai {
		t.Fatalf("CalendarOf(nil) = %s", got)
	}
}
