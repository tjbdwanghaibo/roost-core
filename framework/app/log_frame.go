package app

import "sync/atomic"

// ModLogFrame 是日志帧号来源的能力名。App 每次运行在注册表里放自己的 *LogFrame，驱动帧的 Mod（Nest）在
// Provide 时把自己的帧号接上。
const ModLogFrame ModName = "log.frame"

// LogFrame 给不在 Nest 请求上下文里的日志行提供当前帧号（请求内的日志行由 fctx 带帧号）。
// 没有来源时为 0；Current 可以与 SetSource 并发调用。
type LogFrame struct {
	source atomic.Pointer[func() uint64]
}

// SetSource 接上帧号来源；nil 表示没有来源。
func (f *LogFrame) SetSource(source func() uint64) {
	if source == nil {
		f.source.Store(nil)
		return
	}
	f.source.Store(&source)
}

// Current 返回来源的当前帧号，没有来源时为 0。
func (f *LogFrame) Current() uint64 {
	if source := f.source.Load(); source != nil {
		return (*source)()
	}
	return 0
}
