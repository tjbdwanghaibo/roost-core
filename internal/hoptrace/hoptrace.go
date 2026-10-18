// Package hoptrace 是 saga 原生步骤关键路径的逐跳计时缝（2026-10-18 saga 性能优化第 1 项）。
//
// 只给性能夹具用：生产不安装 Recorder，每个计时点只多一次原子读（nil 就返回），不分配、不读时钟、不打指标。
// 夹具在开测前 Install 一个 Recorder，结束后 Install(nil)。跳名是调用点上的常量字符串（固定的几十个），不带
// saga id 之类的高基数内容；按对象关联两端时间（例如命令发布与消费者收到）用 Mark / Take，键只在 Recorder 里，
// 由 Recorder 负责取走即删、不无界增长。
//
// 计时口径（跳名的含义）写在各调用点的注释里，汇总见 docs/performance/SAGA-OPT-20261018.md。
package hoptrace

import (
	"sync/atomic"
	"time"
)

// Recorder 收集逐跳耗时。实现必须并发安全：调用来自协调器、发布器、各消费者与投影器的 goroutine。
type Recorder interface {
	// Observe 记一跳的一次耗时。
	Observe(hop string, d time.Duration)
	// Mark 记下对象 key 在某一跳结束的时刻，供后一跳 Take 计算跨组件的间隔。
	Mark(key string, at time.Time)
	// Take 取走（并删除）key 的时刻；没有记过返回 false。
	Take(key string) (time.Time, bool)
}

var current atomic.Pointer[Recorder]

// Install 安装（或以 nil 撤销）全局 Recorder。只由性能夹具在测量前后调用。
func Install(recorder Recorder) {
	if recorder == nil {
		current.Store(nil)
		return
	}
	current.Store(&recorder)
}

// Enabled 报告是否安装了 Recorder；调用点用它跳过只为计时才需要的 time.Now。
func Enabled() bool { return current.Load() != nil }

// Since 记一跳：从 start 到现在。start 为零值（调用点没有取时钟）时不记。
func Since(hop string, start time.Time) {
	if recorder := current.Load(); recorder != nil && !start.IsZero() {
		(*recorder).Observe(hop, time.Since(start))
	}
}

// Observe 记一跳的一次耗时。
func Observe(hop string, d time.Duration) {
	if recorder := current.Load(); recorder != nil {
		(*recorder).Observe(hop, d)
	}
}

// Now 在安装了 Recorder 时返回当前时刻，否则返回零值（Since 对零值不记）。调用点写成
// `start := hoptrace.Now(); …; hoptrace.Since(hop, start)`，生产路径上不读时钟。
func Now() time.Time {
	if current.Load() == nil {
		return time.Time{}
	}
	return time.Now()
}

// Mark 记下对象（键 = prefix + id）此刻完成了一跳。键在确认安装了 Recorder 之后才拼接，生产路径不分配。
func Mark(prefix, id string) {
	if recorder := current.Load(); recorder != nil {
		(*recorder).Mark(prefix+id, time.Now())
	}
}

// SinceMark 记一跳：从对象（prefix + id）上次 Mark 到现在，并删除这个 Mark；没有 Mark（未安装、或在 Recorder 安装前
// 标记）时不记。
func SinceMark(hop, prefix, id string) {
	if recorder := current.Load(); recorder != nil {
		if at, ok := (*recorder).Take(prefix + id); ok {
			(*recorder).Observe(hop, time.Since(at))
		}
	}
}

// SinceMarkAt 与 SinceMark 相同，只是终点是给定的 end（例如一段处理开始的时刻）而不是现在。
func SinceMarkAt(hop, prefix, id string, end time.Time) {
	if recorder := current.Load(); recorder != nil {
		if at, ok := (*recorder).Take(prefix + id); ok {
			(*recorder).Observe(hop, end.Sub(at))
		}
	}
}
