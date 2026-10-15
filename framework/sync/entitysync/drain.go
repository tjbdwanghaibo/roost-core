package entitysync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// Drain 在生产者停止后排空已登记的同步工作。预算/水位暂缓时等到下个周期，
// 传输失败或 context 取消明确返回错误；调用方应保留传输和水位来源直到完成。
// 它不承诺客户端已接收字节，传输队列仍需由其拥有者排空。
//
// ctx 超时或取消时返回 *DrainStalledError：errors.Is(err, context.DeadlineExceeded / Canceled) 不变，
// 另外列出仍未排空的条目（subject、会话、最后一次被暂缓或拒绝的原因、已等待时长），条目多时只列前
// DrainStallLimit 项并给总数；同时计 entitysync_drain_timeout_total，并把剩余条目数写入
// entitysync_drain_remaining_entries（成功排空写 0）。这是诊断，Drain 不会因此放弃任何条目：
// 永久拒绝的条目仍要靠修复关系 / 水位，或由调用方在超时后决定继续停机。
func (m *Manager) Drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	for {
		if err := m.Flush(ctx); err != nil {
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return m.drainStalled(err, started)
			}
			return err
		}
		m.pendingMu.Lock()
		pending := len(m.pending) + len(m.waitingSnapshots)
		m.pendingMu.Unlock()
		if pending == 0 && !m.policiesPending() {
			metrics.SetGauge(metricDrainRemaining, nil, 0)
			return nil
		}
		timer := time.NewTimer(m.config.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return m.drainStalled(ctx.Err(), started)
		case <-timer.C:
		}
	}
}

// Drain 诊断指标：次数与剩余条目数都不带标签，基数固定。
const (
	metricDrainTimeout   = "entitysync_drain_timeout_total"
	metricDrainRemaining = "entitysync_drain_remaining_entries"
)

// DrainStallLimit 是 *DrainStalledError 最多列出的条目数；Total 仍是全部条目数。
const DrainStallLimit = 8

// DrainStall 是 Drain 结束时仍未排空的一项。
type DrainStall struct {
	// Source 是条目来源：subject（待捕获的 subject）、snapshot（等冷创建预算的快照）、
	// policy（政策阶段：框架的释放 / 重新提交通知，或某个政策报告的待处理事实）。
	Source string
	// Subject / Session 是条目对应的 subject 与会话，0 表示不针对单个 subject / 会话。
	Subject int64
	Session SessionID
	// Reason 是最后一次被暂缓或拒绝的原因，例如 commit_pending（提交确认未到）、
	// durability_wait（持久水位未到）、prepare_failed、snapshot_budget、subscribe_refused。
	// 空表示已登记、还没有被 Flush 判定过。
	Reason string
	// Detail 是原因的补充（版本、水位、拒绝次数等），可为空。
	Detail string
	// Waited 是自第一次被暂缓 / 拒绝起连续等待的时长，0 表示来源不记录时间。
	Waited time.Duration
	// Err 是最后一次拒绝的错误（若有）。
	Err error
}

func (s DrainStall) String() string {
	var b strings.Builder
	b.WriteString(s.Source)
	if s.Subject != 0 {
		if s.Source != "subject" {
			b.WriteString(" subject")
		}
		fmt.Fprintf(&b, " %d", s.Subject)
	}
	if s.Session != 0 {
		fmt.Fprintf(&b, " session %d", s.Session)
	}
	reason := s.Reason
	if reason == "" {
		reason = "pending"
	}
	b.WriteString(": ")
	b.WriteString(reason)
	if s.Detail != "" {
		fmt.Fprintf(&b, " (%s)", s.Detail)
	}
	if s.Waited > 0 {
		fmt.Fprintf(&b, ", waited %s", s.Waited.Round(time.Millisecond))
	}
	if s.Err != nil {
		fmt.Fprintf(&b, ": %v", s.Err)
	}
	return b.String()
}

// DrainStalledError 是 Drain 因 ctx 结束而未排空时的错误。Unwrap 返回原因（ctx 错误，或带着 ctx 错误的
// Flush 错误），所以 errors.Is(err, context.DeadlineExceeded / Canceled) 照常成立；用 errors.As 取条目。
type DrainStalledError struct {
	Cause   error
	Elapsed time.Duration // Drain 已运行的时长
	Stalls  []DrainStall  // 最多 DrainStallLimit 项：subject 按 ID 升序，其后是快照等待与政策条目
	Total   int           // 全部未排空条目数
}

func (e *DrainStalledError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "entitysync drain stalled after %s with %d entr", e.Elapsed.Round(time.Millisecond), e.Total)
	if e.Total == 1 {
		b.WriteString("y")
	} else {
		b.WriteString("ies")
	}
	for i, stall := range e.Stalls {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(stall.String())
	}
	if more := e.Total - len(e.Stalls); more > 0 {
		fmt.Fprintf(&b, "; and %d more", more)
	}
	fmt.Fprintf(&b, ": %v", e.Cause)
	return b.String()
}

func (e *DrainStalledError) Unwrap() error { return e.Cause }

// drainStalled 收集剩余条目、记指标并包装 cause。
func (m *Manager) drainStalled(cause error, started time.Time) error {
	stalls, total := m.drainStalls(DrainStallLimit)
	metrics.IncCounter(metricDrainTimeout, nil, 1)
	metrics.SetGauge(metricDrainRemaining, nil, int64(total))
	return &DrainStalledError{Cause: cause, Elapsed: time.Since(started), Stalls: stalls, Total: total}
}

// drainStalls 列出仍未排空的条目，最多 limit 项，并返回总数。只读诊断状态：不取 flushGate，
// 不调用政策的 apply；政策的 Stalls 回调与 pending 回调同样只读。
func (m *Manager) drainStalls(limit int) ([]DrainStall, int) {
	now := time.Now()
	var stalls []DrainStall
	total := 0
	add := func(stall DrainStall) {
		total++
		if len(stalls) < limit {
			stalls = append(stalls, stall)
		}
	}
	m.pendingMu.Lock()
	ids := make([]int64, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		stall := DrainStall{Source: "subject", Subject: id}
		if d, ok := m.deferrals[id]; ok {
			stall.Reason, stall.Detail, stall.Waited, stall.Err = d.reason, d.detail, now.Sub(d.since), d.err
		}
		add(stall)
	}
	for element := m.waitingOrder.Front(); element != nil; element = element.Next() {
		id := element.Value.(int64)
		if _, pending := m.pending[id]; pending {
			continue // 已作为 subject 条目列出
		}
		add(DrainStall{Source: "snapshot", Subject: id, Reason: "snapshot_budget", Waited: now.Sub(m.waitingSnapshots[id].since)})
	}
	m.pendingMu.Unlock()

	if n := m.releaseEntries.Load(); n > 0 {
		add(DrainStall{Source: "policy", Reason: "release_notice", Detail: fmt.Sprintf("%d release notice(s) not delivered", n)})
	}
	if m.resubmitsPending() {
		add(DrainStall{Source: "policy", Reason: "resubmit", Detail: "retracted subscriptions waiting to be handed back"})
	}
	for _, hook := range m.policyHooks() {
		if hook.pending == nil || !hook.pending() {
			continue
		}
		if hook.stalls == nil {
			add(DrainStall{Source: "policy", Reason: "policy_pending"})
			continue
		}
		listed, count := hook.stalls(max(limit-len(stalls), 0))
		for i := range count {
			if i < len(listed) {
				add(listed[i])
			} else {
				total++
			}
		}
	}
	return stalls, total
}

// deferral 是一个 subject 连续被 Flush 暂缓的原因与起点，只供 Drain 诊断；pendingMu 保护。
// 被暂缓的 subject 一定重新排进 pending，所以记录随它下一次被处理而更新或删除，不会单独增长。
type deferral struct {
	reason string
	detail string
	since  time.Time
	err    error
}

// deferredSubject 是 Flush 本轮要重新排队的 subject 及原因。
type deferredSubject struct {
	id     int64
	reason string
	detail string
	err    error
}

// settleDeferrals 在 Flush 末尾重新排队被暂缓的 subject 并记下原因：连续暂缓保留第一次的时间；
// 本轮处理过、没有再被暂缓的 subject 删掉旧记录。
func (m *Manager) settleDeferrals(processed []int64, retry []deferredSubject) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	if len(retry) == 0 && len(m.deferrals) == 0 {
		return
	}
	if m.deferrals == nil {
		m.deferrals = make(map[int64]deferral)
	}
	now := time.Now()
	deferred := make(map[int64]struct{}, len(retry))
	for _, item := range retry {
		deferred[item.id] = struct{}{}
		m.markPendingLocked(item.id)
		since := now
		if previous, ok := m.deferrals[item.id]; ok {
			since = previous.since
		}
		m.deferrals[item.id] = deferral{reason: item.reason, detail: item.detail, since: since, err: item.err}
	}
	for _, id := range processed {
		if _, again := deferred[id]; !again {
			delete(m.deferrals, id)
		}
	}
}
