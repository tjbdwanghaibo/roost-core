// Package servicemetrics 提供各业务服务共用的事件上报契约，使队列深度、CAS 冲突、
// 业务拒绝、幂等重放和丢弃可以被观察。领域实现依赖 core，Kit 只保留类型别名与转发。
// 服务通过 Wrap 保存可选 Reporter；它不改变业务结果，也不自动把所有错误转成指标。
package servicemetrics

// Reporter receives service events.
//
// The methods are named after the event rather than taking a metric name,
// because a mistyped name is a metric nobody sees. Adding an event is a
// compile error at every implementation, which is the point: a new silent
// path cannot be introduced without someone deciding how it is observed.
//
// A nil Reporter means "no reporting" and is never a reason for an operation
// to fail. Use Wrap to get a nil-safe value.
type Reporter interface {
	// Accepted counts operations that completed and changed state.
	Accepted(op string)
	// Refused counts operations rejected for a business reason — a limit, a
	// permission, a stale token. Not an infrastructure failure.
	Refused(op, reason string)
	// Replayed counts operations answered from an idempotency record rather
	// than applied again. A rising rate means the transport is redelivering.
	Replayed(op string)
	// Dropped counts records discarded by retention or a bound. This is the
	// drop count: an archive that truncates, a ring that evicts, a queue
	// entry swept for expiry.
	Dropped(op string, count int)
	// Conflict counts compare-and-set exhaustion. Contention is the expected
	// failure mode when one logical key holds one versioned entry, so it has
	// to be visible rather than surfacing as an opaque internal error.
	Conflict(op string)
	// Depth reports a current size — a queue length, a pending window, a
	// board size. It is a gauge, so it is set rather than incremented.
	Depth(name string, value int64)
}

// Wrap returns a nil-safe reporter. A service stores the result of Wrap so
// every call site can be unconditional: guarding each call with a nil check is
// how a report gets forgotten.
func Wrap(reporter Reporter) Sink { return Sink{reporter: reporter} }

// Sink is a nil-safe Reporter.
type Sink struct{ reporter Reporter }

func (s Sink) Accepted(op string) {
	if s.reporter != nil {
		s.reporter.Accepted(op)
	}
}

func (s Sink) Refused(op, reason string) {
	if s.reporter != nil {
		s.reporter.Refused(op, reason)
	}
}

func (s Sink) Replayed(op string) {
	if s.reporter != nil {
		s.reporter.Replayed(op)
	}
}

func (s Sink) Dropped(op string, count int) {
	if s.reporter != nil && count > 0 {
		s.reporter.Dropped(op, count)
	}
}

func (s Sink) Conflict(op string) {
	if s.reporter != nil {
		s.reporter.Conflict(op)
	}
}

func (s Sink) Depth(name string, value int64) {
	if s.reporter != nil {
		s.reporter.Depth(name, value)
	}
}

// Enabled reports whether anything is listening. Use it only to skip work
// that exists solely to produce a report — never to skip the report itself.
func (s Sink) Enabled() bool { return s.reporter != nil }
