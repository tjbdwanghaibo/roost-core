package heartbeat

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

const (
	// DeliveredMetric 计交给 Nest 的拍（批量作业里的目标与单目标 Dispatch；准入被拒的不算），标签 driver。
	DeliveredMetric = "heartbeat.delivered_total"
	// ExecutedMetric 计执行了回调的拍。
	ExecutedMetric = "heartbeat.executed_total"
	// SkippedMetric 计跳过的拍，标签 reason：inflight（上一拍还没执行完）、queue_full（快池准入满，含批量作业里冲突
	// 转单目标被拒）、
	// rejected（其他准入失败，例如引擎停止中）。持续增长说明实体的 worker 忙不过来或回调太慢；同时打限频 Warn。
	SkippedMetric = "heartbeat.skipped_total"
	// StalledMetric 计在途超过 StallAfter 视为丢失而重投的拍；同时打限频 Warn。基线应为零。
	StalledMetric = "heartbeat.stalled_total"
	// DroppedMetric 计到期或执行时按规则丢弃的拍，标签 reason：removed（实例已卸载 / 删档；驱动随即撤销这个实例的
	// 全部登记，之后不再计，同一格里一起到期的登记各计一次）、stale（目标已被同 ID 的新实例取代，实例已 OnDestroy，
	// 或登记已 Close）、not_loaded（实例还没发布）、stopped（停止生效后到达）、not_owner（Remote 托管实体本进程不是
	// 当前拥有方）、duplicate（这一代投递已经结算或正在执行：同一代的另一份已执行、被拒或交回，含下一拍投出后才到的旧重投与旧交回）。
	// 不打日志。
	DroppedMetric = "heartbeat.dropped_total"
	// StoppedMetric 计框架自动停掉的登记，标签 reason：remote_shared（Remote 托管实体进入共享模式，不允许挂载心跳，
	// 见 ErrHeartbeatRemoteShared）。同时汇总打 Warn。
	StoppedMetric = "heartbeat.stopped_total"
	// RegisteredMetric 是正在跳（已启动生效）的登记数（Gauge）。
	RegisteredMetric = "heartbeat.registered"
)

// --- 计数、指标与限频 Warn ---

const (
	cDelivered = iota
	cExecuted
	cSkipInflight
	cSkipQueueFull
	cSkipRejected
	cStalled
	cDropRemoved
	cDropStale
	cDropNotLoaded
	cDropStopped
	cDropNotOwner
	cDropDuplicate
	cStoppedShared
	numCounters
)

// stripedCounter 按实体 ID 分 16 片累加：回调侧计数由全部 worker 并发写，一个原子量会成为热点。
type stripedCounter [16]struct {
	n atomic.Int64
	_ [56]byte
}

func (c *stripedCounter) add(id int64) { c[uint64(id)%16].n.Add(1) }
func (c *stripedCounter) addN(n int64) { c[0].n.Add(n) }
func (c *stripedCounter) load() (sum int64) {
	for i := range c {
		sum += c[i].n.Load()
	}
	return sum
}

type driverStats struct {
	delivered, executed                       stripedCounter
	skipInflight, skipQueueFull, skipRejected stripedCounter
	stalled                                   stripedCounter
	dropRemoved, dropStale, dropNotLoaded     stripedCounter
	dropStopped, dropNotOwner, dropDuplicate  stripedCounter
	stoppedShared                             stripedCounter
}

func (s *driverStats) counters() *[numCounters]*stripedCounter {
	return &[numCounters]*stripedCounter{
		cDelivered: &s.delivered, cExecuted: &s.executed,
		cSkipInflight: &s.skipInflight, cSkipQueueFull: &s.skipQueueFull, cSkipRejected: &s.skipRejected,
		cStalled:     &s.stalled,
		cDropRemoved: &s.dropRemoved, cDropStale: &s.dropStale, cDropNotLoaded: &s.dropNotLoaded,
		cDropStopped: &s.dropStopped, cDropNotOwner: &s.dropNotOwner, cDropDuplicate: &s.dropDuplicate,
		cStoppedShared: &s.stoppedShared,
	}
}

func (d *Driver) count(c *stripedCounter, id int64) { c.add(id) }

// Stats 是驱动计数的快照（测试与诊断用；运维看 metrics）。
type Stats struct {
	Delivered, Executed                                int64
	SkippedInflight, SkippedQueueFull, SkippedRejected int64
	Stalled                                            int64
	DroppedRemoved, DroppedStale, DroppedNotLoaded     int64
	DroppedStopped, DroppedNotOwner, DroppedDuplicate  int64
	StoppedShared                                      int64
	Registered                                         int
}

// Stats 返回计数快照。
func (d *Driver) Stats() Stats {
	s := &d.stats
	return Stats{
		Delivered: s.delivered.load(), Executed: s.executed.load(),
		SkippedInflight: s.skipInflight.load(), SkippedQueueFull: s.skipQueueFull.load(), SkippedRejected: s.skipRejected.load(),
		Stalled:        s.stalled.load(),
		DroppedRemoved: s.dropRemoved.load(), DroppedStale: s.dropStale.load(), DroppedNotLoaded: s.dropNotLoaded.load(),
		DroppedStopped: s.dropStopped.load(), DroppedNotOwner: s.dropNotOwner.load(), DroppedDuplicate: s.dropDuplicate.load(),
		StoppedShared: s.stoppedShared.load(),
		Registered:    d.Registered(),
	}
}

// flush 把计数增量写进 metrics 注册表，并按 WarnInterval 打一条汇总 Warn。只由驱动 goroutine 调用；sysNow 是系统时间。
func (d *Driver) flush(sysNow time.Time) {
	counters := d.stats.counters()
	var delta [numCounters]int64
	for i, c := range counters {
		v := c.load()
		delta[i] = v - d.flushed[i]
		d.flushed[i] = v
	}
	name := d.config.Name
	incCounter(DeliveredMetric, metrics.Labels{"driver": name}, delta[cDelivered])
	incCounter(ExecutedMetric, metrics.Labels{"driver": name}, delta[cExecuted])
	incCounter(SkippedMetric, metrics.Labels{"driver": name, "reason": "inflight"}, delta[cSkipInflight])
	incCounter(SkippedMetric, metrics.Labels{"driver": name, "reason": "queue_full"}, delta[cSkipQueueFull])
	incCounter(SkippedMetric, metrics.Labels{"driver": name, "reason": "rejected"}, delta[cSkipRejected])
	incCounter(StalledMetric, metrics.Labels{"driver": name}, delta[cStalled])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "removed"}, delta[cDropRemoved])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "stale"}, delta[cDropStale])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "not_loaded"}, delta[cDropNotLoaded])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "stopped"}, delta[cDropStopped])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "not_owner"}, delta[cDropNotOwner])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "duplicate"}, delta[cDropDuplicate])
	incCounter(StoppedMetric, metrics.Labels{"driver": name, "reason": "remote_shared"}, delta[cStoppedShared])
	metrics.SetGauge(RegisteredMetric, metrics.Labels{"driver": name}, int64(d.Registered()))
	d.warn.emit(d.config.Logger, name, d.config.WarnInterval, sysNow)
	d.emitSharedWarn(delta[cStoppedShared], sysNow)
}

// emitSharedWarn 为共享模式停掉的登记打 Warn，按 WarnInterval 限频：间隔内只累计，下一条汇总个数并带一个样本。
func (d *Driver) emitSharedWarn(stopped int64, now time.Time) {
	w := &d.sharedWarn
	w.pending += stopped
	if w.pending == 0 || (!w.last.IsZero() && now.Sub(w.last) < d.config.WarnInterval) {
		return
	}
	args := []any{"driver", d.config.Name, "stopped_since_last_warn", w.pending}
	if r := d.sharedSample.Swap(nil); r != nil {
		args = append(args, "sample_entity", r.host.id, "sample_registration", r.name)
	}
	d.config.Logger.Warn("heartbeat: registrations stopped because their Remote entity entered shared mode; "+
		"heartbeats are not allowed in shared mode, drive the periodic logic from business code instead", args...)
	w.last, w.pending = now, 0
}

// incCounter 只写非零增量（flush 每秒对每个原因各调一次，大多是零）。
func incCounter(name string, labels metrics.Labels, delta int64) {
	if delta > 0 {
		metrics.IncCounter(name, labels, delta)
	}
}

// warnState 汇总上一条 Warn 之后的跳拍 / 卡住，按间隔打一条。只由驱动 goroutine 读写。
type warnState struct {
	last    time.Time
	pending map[string]int64
	sample  *Reg
	reason  string
	err     error
}

func (w *warnState) note(r *Reg, reason string) {
	if w.pending == nil {
		w.pending = make(map[string]int64)
	}
	w.pending[reason]++
	w.sample, w.reason = r, reason
}

func (w *warnState) noteErr(r *Reg, reason string, err error) {
	w.note(r, reason)
	w.err = err
}

func (w *warnState) emit(logger *slog.Logger, driver string, interval time.Duration, now time.Time) {
	if len(w.pending) == 0 || (!w.last.IsZero() && now.Sub(w.last) < interval) {
		return
	}
	args := []any{"driver", driver, "since_last_warn", w.pending, "sample_entity", w.sample.host.id,
		"sample_registration", w.sample.name, "sample_reason", w.reason}
	if w.err != nil {
		args = append(args, "sample_err", w.err)
	}
	logger.Warn("heartbeat: beats skipped (previous beat still in flight, admission refused, or presumed lost); "+
		"the entity's worker is falling behind or a callback is slow", args...)
	w.last = now
	w.pending, w.sample, w.err = nil, nil, nil
}
