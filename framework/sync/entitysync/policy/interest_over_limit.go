package policy

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// ---- 超额降级的 Interest 一侧（维护者 2026-11-08，可编辑地形方案 D17） ----
//
// 会话里某一类对象已满时，Manager 不关会话，暂缓多出来的新对象并通知订阅来源（entitysync.SubscriptionSourceHooks.Deferred，
// 见 entitysync/over_limit.go）。Interest 是唯一知道距离的政策，所以由它把“谁留下”收敛到最近的 N 个：
//
//   - 收紧（deferred）：一批通知按玩家的视野归并，每条通知让一个空间视野的可见上限减一——优先这对 pair 所在的、
//     只挂给这名玩家的空间视野；没有就用玩家的主视野；再没有就用 pair 所在的共享空间视野（收紧会让挂接它的其他玩家
//     一起少看）。新上限 = min(当前上限, 当前可见数) − 通知数（不低于 0），AOI 随即完整重评：从最远的成员开始离开，
//     结算时退订。被暂缓的 pair 若本身就在最远的那几个里，退订后它从未到过客户端；否则离开的是更远的已持有对象，
//     腾出的位置在下一次 Flush 由 Manager 放回最近的暂缓对象。通知只说“少一个”，不需要知道 Manager 的类别与上限。
//     关系视野、块覆盖、self 没有距离，不收紧；只来自它们的 pair 让主视野让位。
//   - 放宽（relaxLimitsLocked，每次结算）：先在当前上限内补位（成员离开后腾出的名额立即给次近的目标，不会超额）；
//     冷却期内上限保持。到期后，补位之后可见数仍低于上限（上限已不起作用）就撤掉、回到 AOIConfig.MaxVisible；
//     否则上调一档（MaxVisible 的 1/8，至少 1；不限时按当前上限的 1/8，至少 1），到 MaxVisible 即撤掉。
//   - 滞回：上调之后冷却期内又收到通知，说明边界就在这里——冷却期加倍（overLimitRelaxBase 起，至多 overLimitRelaxMax）；
//     上调之后安然度过冷却期则减半（不低于起点）。所以拥挤持续时上限停在边界附近、试探越来越稀；拥挤散去时上限
//     不再起作用而直接撤掉。失败的试探只是 Manager 内部的一次暂缓（新进入的目标还没创建就被退订），不产生线上流量。
//
// 状态只在应用侧（持 Interest.mu）读写；视野撤出（removeSpatialLocked）时删除。

const (
	overLimitRelaxBase = time.Second
	overLimitRelaxMax  = 32 * time.Second
)

// 指标：overLimitMetric{action} 计收紧（tighten）、上调（raise）、撤掉（lift）与找不到可收紧视野（untightened）的次数；
// limitedViewsMetric 是此刻整个进程里收紧着的空间视野数（各 Interest 之和，正常为 0）。
const (
	overLimitMetric    = "entitysync_interest_over_limit_total"
	limitedViewsMetric = "entitysync_interest_limited_views"
)

// limitedViews 是进程里所有 Interest 收紧着的空间视野数之和（一个场景常有玩家与地形两个 Interest，各写各的会互相覆盖）；
// 加总与写指标在同一把锁里，写入顺序与加总顺序一致。
var limitedViews struct {
	mu    sync.Mutex
	total int64
}

// publishLimitedViewsLocked 把本 Interest 收紧视野数的变化计入 limitedViews 并发布；调用方持有 in.mu。
func (in *Interest) publishLimitedViewsLocked() {
	if delta := len(in.limits) - in.limitsReported; delta != 0 {
		in.limitsReported = len(in.limits)
		limitedViews.mu.Lock()
		limitedViews.total += int64(delta)
		metrics.SetGauge(limitedViewsMetric, nil, limitedViews.total)
		limitedViews.mu.Unlock()
	}
}

// viewLimit 是一个空间视野的收紧状态。
type viewLimit struct {
	limit    int
	cooldown time.Duration
	next     time.Time // 冷却期结束、可以试着放宽的时刻
	raised   bool      // 自上次收紧以来上调过
}

// deferred 是 Interest 来源的 Deferred 回调：在 Manager Flush 的政策阶段（各政策 Apply 之前）调用。收紧随即生效，
// 同一政策阶段里紧接着的结算（settleQueued）把离开视野的 pair 退订。
func (in *Interest) deferred(items []entitysync.DeferredSubscription) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return
	}
	shrink := make(map[int64]int)
	for _, item := range items {
		key := pair{observer: int64(item.Session), subject: item.Subject}
		current := in.held[key]
		if current == nil || !current.subscribed {
			continue // 已经退订，或还没说给 Manager：没有要让的位置
		}
		handle := in.limitedViewForLocked(key, current)
		if handle == 0 {
			metrics.IncCounter(overLimitMetric, metrics.Labels{"action": "untightened"}, 1)
			continue
		}
		shrink[handle]++
	}
	if len(shrink) == 0 {
		return
	}
	now := in.now()
	for _, handle := range slices.Sorted(maps.Keys(shrink)) {
		in.tightenLocked(handle, shrink[handle], now)
	}
	in.publishLimitedViewsLocked()
}

// limitedViewForLocked 选一个为 pair 让位的空间视野（见文件开头的顺序），返回 AOI 观察者编号；0 表示没有。
func (in *Interest) limitedViewForLocked(key pair, current *hold) int64 {
	var private, shared int64
	for source := range current.layers {
		if source.block || source.view == 0 || in.aoi.observers[source.view] == nil {
			continue // self、块覆盖、关系视野没有距离
		}
		players := in.spatialPlayers[source.view]
		_, mine := players[key.observer]
		switch {
		case mine && len(players) == 1:
			if private == 0 || source.view < private {
				private = source.view
			}
		default:
			if shared == 0 || source.view < shared {
				shared = source.view
			}
		}
	}
	if private != 0 {
		return private
	}
	if main := in.registry.mainOf(key.observer); main != nil && in.aoi.observers[main.handle] != nil {
		return main.handle
	}
	return shared
}

// tightenLocked 把空间视野的上限再降 by 个（见文件开头）并让 AOI 重评。
func (in *Interest) tightenLocked(handle int64, by int, now time.Time) {
	observer := in.aoi.observers[handle]
	if observer == nil {
		return
	}
	current := len(observer.visible)
	entry := in.limits[handle]
	switch {
	case entry != nil:
		current = min(current, entry.limit)
	case in.aoi.config.MaxVisible > 0:
		current = min(current, in.aoi.config.MaxVisible)
	}
	if entry == nil {
		entry = &viewLimit{cooldown: overLimitRelaxBase}
		in.limits[handle] = entry
	} else if entry.raised {
		// 上调之后又超额：边界就在这里，下次隔更久再试。
		entry.cooldown = min(2*entry.cooldown, overLimitRelaxMax)
	}
	entry.limit, entry.raised, entry.next = max(0, current-by), false, now.Add(entry.cooldown)
	_ = in.aoi.SetObserverLimit(handle, entry.limit, true)
	metrics.IncCounter(overLimitMetric, metrics.Labels{"action": "tighten"}, 1)
}

// relaxLimitsLocked 是每次结算开头的放宽（见文件开头）。按视野编号顺序处理，结果确定。
func (in *Interest) relaxLimitsLocked(now time.Time) {
	if len(in.limits) == 0 {
		return
	}
	for _, handle := range slices.Sorted(maps.Keys(in.limits)) {
		entry := in.limits[handle]
		if in.aoi.observers[handle] == nil {
			delete(in.limits, handle)
			continue
		}
		// 先在当前上限内补位（成员离开后 AOI 不会立即补，见 AOIConfig.MaxVisible）：不放宽上限，不会超额。
		crowded := in.aoi.observerCrowded(handle)
		if now.Before(entry.next) {
			continue
		}
		if entry.raised {
			// 上一次上调安然度过了冷却期：试探可以密一点。
			entry.cooldown = max(entry.cooldown/2, overLimitRelaxBase)
		}
		configured := in.aoi.config.MaxVisible
		if !crowded {
			in.liftLimitLocked(handle)
			continue
		}
		base := configured
		if base <= 0 {
			base = entry.limit
		}
		next := entry.limit + max(1, base/8)
		if configured > 0 && next >= configured {
			in.liftLimitLocked(handle)
			continue
		}
		entry.limit, entry.raised, entry.next = next, true, now.Add(entry.cooldown)
		_ = in.aoi.SetObserverLimit(handle, next, true)
		metrics.IncCounter(overLimitMetric, metrics.Labels{"action": "raise"}, 1)
	}
	in.publishLimitedViewsLocked()
}

// liftLimitLocked 撤掉收紧，视野回到 AOIConfig.MaxVisible。
func (in *Interest) liftLimitLocked(handle int64) {
	delete(in.limits, handle)
	_ = in.aoi.SetObserverLimit(handle, 0, false)
	metrics.IncCounter(overLimitMetric, metrics.Labels{"action": "lift"}, 1)
}

// forgetLimitLocked 在空间视野撤出时删除它的收紧状态。
func (in *Interest) forgetLimitLocked(handle int64) {
	if _, limited := in.limits[handle]; limited {
		delete(in.limits, handle)
		in.publishLimitedViewsLocked()
	}
}
