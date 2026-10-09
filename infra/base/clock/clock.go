// Package clock 是业务时钟（维护者决定 D-L3，docs/feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md）。
//
// 时间分两个钟，边界写死：
//
//   - 业务时钟 = 真实时间 + time.logic_offset。玩法与业务时间读它：活动窗口与协调器、World 定时器、
//     日 / 周重置、冷却、邮件 / 道具业务过期、赛季、排行周期、skill / 战斗里的游戏时间。
//   - 系统时钟 = 真实时间，直接用 time 包：server 帧率、租约与锁、超时与 ctx 截止、重试退避、存储 TTL、
//     消息 Ack、日志、指标、WAL 与审计时间戳。
//
// 偏移只有一个配置来源 time.logic_offset，App 在启动时读一次（生产环境强制为 0），运行期没有修改入口。
// 同一套部署的业务时间只许前进：偏移可以前拨，不能让业务时间回到部署已经到过的时刻——App 用存在协调
// 存储里的高水位在启动时拒绝（docs/feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md），要回到过去只能清库重建。
// 因此业务服务内部、只与业务时间比较的退避和租约（activity 派发退避、mail 领取租约）也读业务时钟；
// 依赖存储服务端 TTL、与读系统时钟的进程比较、安全有效期、空间回收、审计的才是系统时钟。
// 业务代码从 app.BusinessClock(registry) 拿 Business，或由服务的 Config.Now 注入；本包的全局函数
// （Now / UnixMilli）是进程级业务时钟，给 fctx 与框架库在没有注入时做缺省。
package clock

import (
	"sync/atomic"
	"time"
)

// Business 是业务时钟：读到的是真实时间加上启动时配置的 time.logic_offset。
// 系统时钟没有对应类型，就是 time 包。
type Business interface {
	Now() time.Time
}

// BusinessFunc 把一个函数当作业务时钟，测试替换用。
type BusinessFunc func() time.Time

// Now 实现 Business。
func (f BusinessFunc) Now() time.Time { return f() }

// offsetBusiness 是固定偏移的业务时钟。偏移在构造时定下，之后不能改。
type offsetBusiness struct{ offset time.Duration }

// NewBusiness 返回“真实时间 + offset”的业务时钟。offset 为 0 时 Now 就是 time.Now()。
func NewBusiness(offset time.Duration) Business { return offsetBusiness{offset: offset} }

func (c offsetBusiness) Now() time.Time {
	now := time.Now()
	if c.offset == 0 {
		return now
	}
	return now.Add(c.offset)
}

// processBusiness 读进程级偏移（App 启动时 SetOffset 设下）。
type processBusiness struct{}

func (processBusiness) Now() time.Time { return global.Now() }

// Process 返回进程级业务时钟：偏移是 App 启动时从 time.logic_offset 设下的那个。
// 没有 Registry 可用的框架库（timer、ai、actionflow 的缺省）与 app.BusinessClock 的兜底用它。
func Process() Business { return processBusiness{} }

// Clock 是可设置偏移的逻辑时钟（进程级业务时钟的实现）。业务代码只读时间，用 Business
// （app.BusinessClock）；偏移只在启动时由 App 设置，运行期不改（D-L3）。
type Clock interface {
	Now() time.Time
	UnixMilli() int64
	Set(now time.Time)
	SetOffset(offset time.Duration)
	Offset() time.Duration
	Reset()
}

type logicClock struct {
	offsetNanos atomic.Int64
}

var global Clock = NewLogicClock()

func NewLogicClock() Clock {
	return &logicClock{}
}

// Now 是进程级业务时钟的当前时间（真实时间 + 启动时设下的偏移）。
func Now() time.Time {
	return global.Now()
}

func UnixMilli() int64 {
	return global.UnixMilli()
}

// Set 把进程级业务时钟对到 now。只给测试用。
func Set(now time.Time) {
	global.Set(now)
}

// SetOffset 设置进程级业务时钟的偏移。由 App 在启动时按 time.logic_offset 调用一次；
// 运行期不许调用（D-L3：偏移只在启动时生效），测试可以调用并在结束时恢复。
func SetOffset(offset time.Duration) {
	global.SetOffset(offset)
}

func Offset() time.Duration {
	return global.Offset()
}

// Reset 把进程级偏移清零。只给测试用。
func Reset() {
	global.Reset()
}

func (c *logicClock) Now() time.Time {
	return time.Now().Add(c.Offset())
}

func (c *logicClock) UnixMilli() int64 {
	return c.Now().UnixMilli()
}

func (c *logicClock) Set(now time.Time) {
	c.SetOffset(now.Sub(time.Now()))
}

func (c *logicClock) SetOffset(offset time.Duration) {
	c.offsetNanos.Store(int64(offset))
}

func (c *logicClock) Offset() time.Duration {
	return time.Duration(c.offsetNanos.Load())
}

func (c *logicClock) Reset() {
	c.offsetNanos.Store(0)
}
