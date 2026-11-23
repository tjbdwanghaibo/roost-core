package nestgame

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

const loadLeaveRadius = 180

var sessionObjectLimit = frame.DefaultLimits().MaxObjects

// 仅在业务未启动或已 Shutdown 后核对，此时提交的位置已定，但政策仍可收紧/放宽距离集合。
// 这个场景只有跟随自身的主视野、self 和玩家目标额外声明的块覆盖，不用 Seen 为每个玩家遍历全部 held。
// 每轮在首个 Visible（排空已提交的移动日志）之后重读覆盖，按块倒排。视野订阅 leave 方框相交的块，
// 与 AOI.resubscribe 相同；相同块窗口的玩家共享本轮的块成员，但绝不跨轮缓存成员或距离集合。
type roundCoverage struct {
	byBlock  map[int64][]int64
	byWindow map[[2]int64]map[int64]struct{}
}

func (load *syncGameLoad) coverage(ctx context.Context) (*roundCoverage, error) {
	c := &roundCoverage{byBlock: make(map[int64][]int64), byWindow: make(map[[2]int64]map[int64]struct{})}
	for i := range load.clients { // changed 只有玩家 Unit 声明额外覆盖
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := load.units[i].ID()
		for _, block := range load.interest.ObservedBlocks(id) {
			c.byBlock[block] = append(c.byBlock[block], id)
		}
	}
	return c, nil
}

func (c *roundCoverage) members(layout *spatial.BlockLayout, at spatial.Point) map[int64]struct{} {
	if len(c.byBlock) == 0 {
		return nil
	}
	box, _ := layout.ClampRect(spatial.Rect{
		Min: spatial.Point{X: at.X - loadLeaveRadius, Y: at.Y - loadLeaveRadius},
		Max: spatial.Point{X: at.X + loadLeaveRadius + 1, Y: at.Y + loadLeaveRadius + 1},
	})
	key := [2]int64{layout.BlockIndex(box.Min), layout.BlockIndex(spatial.Point{X: box.Max.X - 1, Y: box.Max.Y - 1})}
	if members, ok := c.byWindow[key]; ok {
		return members
	}
	members := make(map[int64]struct{})
	blocks, _ := layout.BlockRects(box)
	for block := range blocks {
		for _, id := range c.byBlock[block] {
			members[id] = struct{}{}
		}
	}
	c.byWindow[key] = members
	return members
}

// visible 是 Interest.Visible 返回的升序、无重复的距离成员；covered 是本轮的只读块成员。
// 本夹具每个玩家都经 self 看见自己的 Unit（不是会话 ID）；与距离、块重叠时只算一次。
// 即使会话顶满，这个钉住对象也必选；其余名额允许任意合法非钉住对象，不指定暂缓谁。
type settleWanted struct {
	self    int64
	visible []int64
	covered map[int64]struct{}
}

func (w settleWanted) uncovered() iter.Seq[int64] {
	return func(yield func(int64) bool) {
		if _, ok := w.covered[w.self]; !ok && !yield(w.self) {
			return
		}
		for _, id := range w.visible {
			if _, ok := w.covered[id]; !ok && id != w.self && !yield(id) {
				return
			}
		}
	}
}
func (w settleWanted) count() int {
	n := len(w.covered)
	for range w.uncovered() {
		n++
	}
	return n
}
func (w settleWanted) contains(id int64) bool {
	if _, ok := w.covered[id]; ok || id == w.self {
		return true
	}
	_, ok := slices.BinarySearch(w.visible, id)
	return ok
}

type settleMismatch struct {
	index, seen, want, got int
	id                     int64 // 首个 DAO 值不符的对象
	valueMismatch          bool
	expected, actual       [2]int64 // X/HP
	wanted                 settleWanted
	held                   map[int64]syncClientObject
}

// 锁内没有 Interest/Manager 调用、订户排序或格式化。数量和逐对象成员/DAO 值缺一不可；
// 只为本轮首错保留核对当时的持有集合，用于可能的终止诊断，不为每个缺口构造报告。
func (load *syncGameLoad) checkClient(index int, wanted settleWanted, keepFirst bool) (settleMismatch, bool) {
	seen := wanted.count()
	client := &load.clients[index]
	client.mu.Lock()
	defer client.mu.Unlock()
	m := settleMismatch{index: index, seen: seen, want: min(seen, sessionObjectLimit), got: len(client.objects)}
	_, hasSelf := client.objects[wanted.self]
	if m.want == m.got && hasSelf {
		match := true
		for id, obj := range client.objects {
			if !wanted.contains(id) {
				match = false
				break
			}
			u := load.units[entity.GetUniqueIDFromEntityID(id)-1]
			x, hp := u.state.GetX(), u.state.GetHP()
			if obj.value.X != x || obj.value.HP != hp {
				match = false
				m.id, m.valueMismatch = id, true
				m.expected, m.actual = [2]int64{x, hp}, [2]int64{obj.value.X, obj.value.HP}
				break
			}
		}
		if match {
			return m, true
		}
	}
	if keepFirst {
		m.wanted, m.held = wanted, maps.Clone(client.objects)
	}
	return m, false
}

// 只在最终失败调用；锁外只查少数候选（超额时未持有的成员不一定是缺口）。
func (m settleMismatch) detail(subscribed func(int64) bool) string {
	const limit = 4
	var absent, extra []string
	self := m.wanted.self
	missingSelf := ""
	remaining := limit
	if _, ok := m.held[self]; !ok {
		// 确定缺失的钉住对象要单列，不能淹没在合法暂缓的候选中；订户查询仍最多 limit 次。
		missingSelf = fmt.Sprintf(" missing pinned self=%d(subscribed=%v)", self, subscribed(self))
		remaining--
	}
	for _, group := range []iter.Seq[int64]{m.wanted.uncovered(), maps.Keys(m.wanted.covered)} {
		for id := range group {
			if len(absent) == remaining {
				break
			}
			if id == self {
				continue
			}
			if _, ok := m.held[id]; !ok {
				absent = append(absent, fmt.Sprintf("%d(subscribed=%v)", id, subscribed(id)))
			}
		}
	}
	for id := range m.held {
		if !m.wanted.contains(id) {
			extra = append(extra, fmt.Sprint(id))
			if len(extra) == limit {
				break
			}
		}
	}
	value := ""
	if m.valueMismatch {
		value = fmt.Sprintf(" subject=%d want X/HP=%v got=%v", m.id, m.expected, m.actual)
	}
	return fmt.Sprintf("client=%d want count=%d (seen %d, session limit %d) got=%d%s absent candidates=%v extra=%v%s", m.index, m.want, m.seen, sessionObjectLimit, m.got, missingSelf, absent, extra, value)
}

type settleRound struct {
	checked, behind, short int
	minHeld, maxHeld       int
	first                  *settleMismatch
}

// 普通轮一旦证明不匹配就结束；成功必须遍历全部客户端。每 10s 的汇总轮走完同一次遍历，
// behind 包括等数量时的错成员/错值，short 只计封顶后实际差的数量。
func (load *syncGameLoad) scanViews(ctx context.Context, summary bool) (settleRound, error) {
	r, err := load.checkViews(ctx, summary, false)
	if err != nil || r.first != nil || load.self || load.config.ObservedBlocks == 0 {
		return r, err
	}
	// 块计算是夹具的快速判断，不授权通过。成功前必须用政策实际归并的 held + self 重验全员。
	return load.checkViews(ctx, summary, true)
}
func (load *syncGameLoad) checkViews(ctx context.Context, summary, authoritative bool) (r settleRound, err error) {
	var coverage *roundCoverage
	for i := range load.clients {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		u := load.units[i]
		w := settleWanted{self: u.ID()}
		if !load.self {
			observer, ok := load.observerOf(w.self)
			if !ok {
				return r, fmt.Errorf("player unit %d has no session", w.self)
			}
			if authoritative {
				w.visible = load.interest.Seen(observer)
			} else {
				w.visible = load.interest.Visible(observer)
			}
			if !authoritative && load.config.ObservedBlocks > 0 {
				if coverage == nil {
					coverage, err = load.coverage(ctx)
					if err != nil {
						return r, err
					}
				}
				w.covered = coverage.members(&load.layout, load.position(u))
			}
		}
		m, ok := load.checkClient(i, w, r.first == nil)
		if i == 0 || m.got < r.minHeld {
			r.minHeld = m.got
		}
		r.maxHeld = max(r.maxHeld, m.got)
		r.checked++
		if ok {
			continue
		}
		r.behind++
		r.short += max(0, m.want-m.got)
		if r.first == nil {
			r.first = &m
		}
		if !summary {
			break
		}
	}
	return r, nil
}

type settleFlush struct {
	calls     uint64
	ms, avgMS float64
}

func flushBetween(before, after entitysync.ManagerCounters) settleFlush {
	f := settleFlush{calls: after.FlushCalls - before.FlushCalls, ms: float64(after.FlushDuration-before.FlushDuration) / float64(time.Millisecond)}
	if f.calls != 0 {
		f.avgMS = f.ms / float64(f.calls)
	}
	return f
}

func (load *syncGameLoad) waitViews(ctx context.Context, network loadFrontend, logf func(string, ...any)) (last settleRound, err error) {
	began := time.Now()
	nextReport := began.Add(10 * time.Second)
	previous := load.manager.Counters()
	for {
		if err := network.currentFailure(); err != nil {
			return last, err
		}
		if err := ctx.Err(); err != nil {
			return last, err
		}
		summary := !time.Now().Before(nextReport)
		r, err := load.scanViews(ctx, summary)
		if err == nil || r.first != nil {
			last = r
		}
		// 网络失败、扫描或 Flush 跨过 deadline 都不能因“值相等”而报成功。
		if failure := network.currentFailure(); failure != nil {
			return last, failure
		}
		if err != nil {
			return last, err
		}
		if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
			return last, context.DeadlineExceeded
		}
		if err := ctx.Err(); err != nil {
			return last, err
		}
		if r.first == nil {
			return r, nil
		}
		if summary {
			now := load.manager.Counters()
			f := flushBetween(previous, now)
			// CheckHealth 的 Stats 读增量计数与各 lane 的长度，不遍历全部订户；保留历史 pending / over_limit_waiting 口径。
			logf("Sync settling %s: clients behind=%d short objects=%d frames=%d manager{%s} interval flush_calls=%d flush_ms=%.3f avg_flush_ms=%.3f over_limit_deferred=%d", time.Since(began).Round(time.Second), r.behind, r.short, load.frames.Load(), load.manager.CheckHealth(ctx).Message, f.calls, f.ms, f.avgMS, now.OverLimitDeferred-previous.OverLimitDeferred)
			previous, nextReport = now, time.Now().Add(10*time.Second)
		}
		if err := load.manager.Flush(ctx); err != nil {
			return last, errors.Join(err, network.currentFailure())
		}
		select {
		case <-ctx.Done():
			return last, errors.Join(ctx.Err(), network.currentFailure())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (load *syncGameLoad) awaitValues(t *testing.T, network loadFrontend) (time.Duration, settleFlush, settleRound) {
	t.Helper()
	began := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), began.Add(load.config.Settle))
	defer cancel()
	before := load.manager.Counters()
	r, err := load.waitViews(ctx, network, t.Logf)
	settled := time.Since(began)
	if err == nil && settled > load.config.Settle {
		err = context.DeadlineExceeded
	}
	f := flushBetween(before, load.manager.Counters())
	if err != nil {
		if m := r.first; m != nil {
			observer, _ := load.observerOf(load.units[m.index].ID())
			if load.self {
				observer = int64(m.index + 1)
			}
			t.Logf("Sync mismatch: %s", m.detail(func(id int64) bool {
				return slices.Contains(load.manager.Subscribers(id), entitysync.SessionID(observer))
			}))
		}
		t.Fatalf("Sync final view/DAO values did not converge: %v (checked=%d/%d, frames=%d updates=%d settle=%s flush_calls=%d flush_ms=%.3f avg_flush_ms=%.3f)", err, r.checked, len(load.clients), load.frames.Load(), load.updates.Load(), settled, f.calls, f.ms, f.avgMS)
	}
	return settled, f, r
}
