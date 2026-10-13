package nest

import (
	"sync"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 派发器指标序列的生命周期。
//
// nest.dispatch.* 里带 dispatcher 标签的序列属于以这个名字运行的派发器：OnInit 时登记，
// OnDestroyWithContext 排空返回 nil 之后撤销；以这个名字登记的最后一个派发器撤销时删除这些序列
// （metrics.DeleteSeries），之后 /metrics 里不再有它们。没有删除时序列停在最后的值，每创建一个
// 新名字的派发器就多一组，直到碰到每指标的序列上限。
//
// 按名字计数而不是“谁销毁谁删”：生产里 NestMgr 的派发器都叫 "nest"，同一进程里两个同名派发器
// 写的是同一组序列，先销毁的那个不能删掉另一个还在报的序列。停机超时（返回 ctx 错误）不撤销：
// worker 仍在跑、还会写这些序列，重试排空之后再删，与停止入口三步一致。
//
// 删除与上报在同一把锁下互斥：上报前确认本派发器仍持有序列，所以一次与撤销并发的上报不会在
// 删除之后把序列重新建出来。上报每 1024 次投递一次，这把包级锁不在热路径上。

// dispatcherSeriesNames 列出全部带 dispatcher 标签的指标。新增这类指标必须加到这里，否则它会比派发器活得更久。
var dispatcherSeriesNames = [...]string{
	"nest.dispatch.short_continuations",
	"nest.dispatch.delayed_messages",
	"nest.dispatch.queue_len",
	"nest.dispatch.worker_num",
	"nest.dispatch.io_reroute.total",
}

var dispatcherSeries struct {
	mu sync.Mutex
	// live 按派发器名字记录持有该组序列的派发器个数。
	live map[string]int
}

// holdSeries 登记 m 以自己的名字上报序列。OnInit 调用；中间没有 release 的重复调用无效果。
func (m *Dispatcher) holdSeries() {
	dispatcherSeries.mu.Lock()
	defer dispatcherSeries.mu.Unlock()
	if m.seriesName != nil {
		return
	}
	name := m.Name
	m.seriesName = &name
	if dispatcherSeries.live == nil {
		dispatcherSeries.live = map[string]int{}
	}
	dispatcherSeries.live[name]++
}

// releaseSeries 在 m 排空后撤销 holdSeries；同名的其他派发器都已撤销时删除这组序列。
func (m *Dispatcher) releaseSeries() {
	dispatcherSeries.mu.Lock()
	defer dispatcherSeries.mu.Unlock()
	if m.seriesName == nil {
		return
	}
	name := *m.seriesName
	m.seriesName = nil
	if dispatcherSeries.live[name]--; dispatcherSeries.live[name] > 0 {
		return
	}
	delete(dispatcherSeries.live, name)
	for _, metric := range dispatcherSeriesNames {
		metrics.DeleteSeries(metric, metrics.Labels{"dispatcher": name})
	}
}

// reportSeries 只在 m 持有序列期间执行 report。
func (m *Dispatcher) reportSeries(report func(name string)) {
	dispatcherSeries.mu.Lock()
	defer dispatcherSeries.mu.Unlock()
	if m.seriesName != nil {
		report(*m.seriesName)
	}
}
