package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

type outboxPublication struct {
	status     entity.RemoteCommitStatus
	waiting    int
	dependents []int
}

type outboxPublicationResult struct {
	index int
	err   error
}

// outboxStragglerWait：后台发布循环的一页里已经没有可派发的事务、在途的又这么久没有任何一项结束时，把在途的发布转到
// 后台继续，自己去读下一页。一笔慢发布（一次几秒的 Mongo 写、一次慢的总线确认）因此最多让之后的无关提交多等这么久，
// 而不是等到它结束——修复前单实例 128 并发时全部写额度都停在“等确认”，到 Nest 请求期限同时超时。转后台的代价只是
// 之后几次扫描多读这几条记录（它们在标记已发布前仍是 Applied）；正常发布在几十毫秒内结束，页内总有项在结束，计时一直重置。
const outboxStragglerWait = 100 * time.Millisecond

// publishOutboxPage 由持有outboxSerial的唯一协调者调用；一页最多256项。
// 先按全部Entity建立前序依赖，再只把就绪事务交给有界并发（outboxSlots，与转到后台的慢项共用）；等待前序的事务
// 不占额度，因此热点链不能堵住同页无关事务。失败保留Applied，后继等本次尝试结束再尝试，沿用原串行循环的失败语义。
//
// 在途登记（outboxInflight / outboxBusy）：正在发布的事务（上一页转到后台的慢项、并发的显式恢复派发的）这一页不再派发，
// 涉及在途实体的事务和排在它们后面的同实体事务也留下——同一实体的发布顺序不变；在途发布结束时唤醒发布循环，留下的
// 由那次扫描发布。
//
// background 只由后台发布循环（runOutboxRepublish）置真：页里已没有可派发的事务、在途的超过 outboxStragglerWait 没有
// 任何一项结束时返回，在途的继续在后台完成（各自登记 retryWG，停机等它们结束；失败照常把 tracker 置为 Indeterminate，
// 并记入 outboxDetachedErr 让发布循环退避）。发布用的 ctx 不随本页返回取消：后台模式每笔取 finalizeCtx 加 OpTimeout。
// 显式恢复（RecoverOutbox）等本页派发的全部结束，语义同前。
func (m *Manager) publishOutboxPage(ctx context.Context, pending []entity.RemoteCommitStatus, background bool) error {
	state := m.remote
	slots := m.outboxPublishSlots()
	publications := make([]outboxPublication, len(pending))
	lastEntity := make(map[int64]int)
	lastTransaction := make(map[entity.RemoteTransactionID]int)
	ready := make([]int, 0, len(pending))
	state.outboxMu.Lock()
	for i, status := range pending {
		publications[i].status = status
		predecessors := make(map[int]struct{})
		if previous, found := lastTransaction[status.TransactionID]; found {
			predecessors[previous] = struct{}{}
		}
		lastTransaction[status.TransactionID] = i
		_, blocked := state.outboxInflight[status.TransactionID]
		for _, commit := range status.Commits {
			if previous, found := lastEntity[commit.EntityID]; found && previous != i {
				predecessors[previous] = struct{}{}
			}
			lastEntity[commit.EntityID] = i
			if state.outboxBusy[commit.EntityID] > 0 {
				blocked = true
			}
		}
		for previous := range predecessors {
			publications[previous].dependents = append(publications[previous].dependents, i)
		}
		publications[i].waiting = len(predecessors)
		if blocked {
			// 多计一个永远不会到来的前序：本页不派发它，依赖它的同实体后继也一起留下。
			publications[i].waiting++
		}
		if publications[i].waiting == 0 {
			ready = append(ready, i)
		}
	}
	state.outboxMu.Unlock()

	// results 容量等于页大小，发布 goroutine 交结果永不阻塞；detached 之后它们改为自己收尾（见 finish）。
	results := make(chan outboxPublicationResult, len(pending))
	var detachMu sync.Mutex
	detached := false
	finish := func(index int, err error) {
		detachMu.Lock()
		if !detached {
			results <- outboxPublicationResult{index: index, err: err}
			detachMu.Unlock()
			return
		}
		detachMu.Unlock()
		if err != nil {
			state.outboxMu.Lock()
			state.outboxDetachedErr = errors.Join(state.outboxDetachedErr, fmt.Errorf("remote_entity: recover transaction %s: %w", pending[index].TransactionID, err))
			state.outboxMu.Unlock()
		}
		// 留在 outbox 里等它的同实体后继、失败后的重试都靠这次唤醒。
		m.scheduleOutboxRepublish()
	}
	wait := outboxStragglerWait
	if state.stragglerWait > 0 {
		wait = state.stragglerWait
	}
	var straggle *time.Timer
	defer func() {
		if straggle != nil {
			straggle.Stop()
		}
	}()
	var failures error
	inFlight := 0
	for len(ready) > 0 || inFlight > 0 {
		var dispatch chan<- struct{}
		if len(ready) > 0 && ctx.Err() == nil {
			dispatch = slots
		}
		if dispatch == nil && inFlight == 0 {
			break // 取消后不再派发；已派发任务必须全部结束才交还生命周期（后台模式除外，见上）。
		}
		var canceled <-chan struct{}
		if dispatch != nil {
			canceled = ctx.Done() // 额度被别的在途发布占满时，取消要能打断等待
		}
		var straggling <-chan time.Time
		if background && dispatch == nil {
			if straggle == nil {
				straggle = time.NewTimer(wait)
			}
			straggling = straggle.C
		}
		select {
		case dispatch <- struct{}{}:
			index := ready[0]
			ready = ready[1:]
			inFlight++
			m.startOutboxPublication(ctx, publications[index].status, background, func(err error) { finish(index, err) })
			continue // 派发不算在途项的进展，不重置计时
		case result := <-results:
			inFlight--
			item := &publications[result.index]
			if result.err != nil {
				failures = errors.Join(failures, fmt.Errorf("remote_entity: recover transaction %s: %w", item.status.TransactionID, result.err))
			}
			for _, next := range item.dependents {
				publications[next].waiting--
				if publications[next].waiting == 0 {
					ready = append(ready, next)
				}
			}
		case <-straggling:
			straggle = nil
			detachMu.Lock()
			detached = true
			detachMu.Unlock()
			// 置位前已交来的结果照常处理；它们的同实体后继留给下一次扫描。
			for drained := false; !drained; {
				select {
				case result := <-results:
					inFlight--
					if result.err != nil {
						failures = errors.Join(failures, fmt.Errorf("remote_entity: recover transaction %s: %w", publications[result.index].status.TransactionID, result.err))
					}
				default:
					drained = true
				}
			}
			if inFlight > 0 {
				metrics.IncCounter("remote_entity.outbox.detached_total", nil, int64(inFlight))
			}
			return errors.Join(failures, ctx.Err())
		case <-canceled:
			continue
		}
		// 有在途项结束：重置“没有进展”的计时。
		if straggle != nil {
			straggle.Stop()
			straggle = nil
		}
	}
	return errors.Join(failures, ctx.Err())
}

// startOutboxPublication 在已取得 outboxSlots 额度后登记并发布一笔事务，结束时交还额度与登记，再调用 report。
// 调用方（recoverOutbox）持有一份 retryWG，这里再为 goroutine 登记一份：转到后台后它可能比本页活得久，停机仍要等它。
// 失败时由这里把 tracker 置为 Indeterminate（无论本页是否还在等它），持久记录仍是 Applied，之后重试。
func (m *Manager) startOutboxPublication(ctx context.Context, status entity.RemoteCommitStatus, background bool, report func(error)) {
	state := m.remote
	state.outboxMu.Lock()
	if state.outboxInflight == nil {
		state.outboxInflight = make(map[entity.RemoteTransactionID]struct{})
		state.outboxBusy = make(map[int64]int)
	}
	state.outboxInflight[status.TransactionID] = struct{}{}
	for _, commit := range status.Commits {
		state.outboxBusy[commit.EntityID]++
	}
	state.outboxMu.Unlock()
	state.retryWG.Add(1)
	go func() {
		defer state.retryWG.Done()
		publishCtx, cancel := ctx, context.CancelFunc(func() {})
		if background {
			timeout := m.cfg.OpTimeout
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			publishCtx, cancel = context.WithTimeout(state.finalizeCtx, timeout)
		}
		err := publishCtx.Err()
		if err == nil {
			err = m.publishAppliedRemoteTransaction(publishCtx, status)
		}
		cancel()
		if err != nil {
			m.completeRemoteTransaction(status.TransactionID, entity.RemoteCommitStatus{TransactionID: status.TransactionID, State: entity.RemoteCommitIndeterminate, Receipts: status.Receipts, Cause: err.Error()})
		}
		state.outboxMu.Lock()
		delete(state.outboxInflight, status.TransactionID)
		for _, commit := range status.Commits {
			if state.outboxBusy[commit.EntityID]--; state.outboxBusy[commit.EntityID] <= 0 {
				delete(state.outboxBusy, commit.EntityID)
			}
		}
		state.outboxMu.Unlock()
		<-state.outboxSlots
		report(err)
	}()
}

// outboxPublishSlots 返回在途发布的并发额度（容量 OutboxPublishWorkers：零值取 32，上限 64），首次使用时建立。
func (m *Manager) outboxPublishSlots() chan struct{} {
	state := m.remote
	state.outboxMu.Lock()
	defer state.outboxMu.Unlock()
	if state.outboxSlots == nil {
		workers := m.cfg.OutboxPublishWorkers
		if workers <= 0 {
			workers = 32
		}
		state.outboxSlots = make(chan struct{}, min(workers, 64))
	}
	return state.outboxSlots
}

// takeDetachedOutboxError 取走转到后台的发布失败（没有时为 nil）。
func (m *Manager) takeDetachedOutboxError() error {
	state := m.remote
	state.outboxMu.Lock()
	defer state.outboxMu.Unlock()
	err := state.outboxDetachedErr
	state.outboxDetachedErr = nil
	return err
}

// selectOutboxPage 决定这一页里哪些提交现在由本进程发布（发布归属），并返回留下的提交里最早的到期时刻。
//
// 每个进程的协调者都会扫到共享 outbox 里的全部未发布提交，但 tracker 只在写者进程里：别的进程代发后，写者的 strict 写
// 没人通知，会等到请求期限超时、延迟收尾持写门到 FinalizeProjectionTimeout。所以：
//   - 本进程写的（WriterSID 等于本进程 sid）立即发布；存储不记录写者（WriterSID 为 0）或没有写入时刻的，同样立即发布；
//     同 sid 重启的新进程接手旧进程留下的提交也走这里——单实例锁保证旧进程已经不在。
//   - 别的进程写的，写入后过了宽限期（outboxTakeoverGrace）仍未发布，视为写者已经不在，由本进程代发（reason=grace）。
//   - 同一实体上排在它后面、这一轮要发布的提交已经存在时立即代发（reason=successor）：同一实体的提交在写门与写锁下
//     串行，写者在提交发布前一直持有它们，后面能有别的提交，说明前面的写者已经失去这个实体（崩溃后被接管）；
//     一并发布保持同一实体的发布顺序，接管者的写也不用陪着等宽限期。只在页内判断；跨页时后面的提交照常先发
//     （与发布失败时一样，同实体顺序只在一页内尽力保持）。
func (m *Manager) selectOutboxPage(pending []entity.RemoteCommitStatus) ([]entity.RemoteCommitStatus, time.Time) {
	now := time.Now
	if m.remote.outboxNow != nil {
		now = m.remote.outboxNow
	}
	at := now()
	grace := m.outboxTakeoverGrace()
	publish := make([]bool, len(pending))
	deferred := 0
	for i := range pending {
		status := &pending[i]
		switch {
		case status.WriterSID == 0 || status.WriterSID == m.localSid || status.CreatedAt.IsZero():
			publish[i] = true
		case !at.Before(status.CreatedAt.Add(grace)):
			publish[i] = true
			metrics.IncCounter("remote_entity.outbox.takeover_total", metrics.Labels{"reason": "grace"}, 1)
		default:
			deferred++
		}
	}
	if deferred == 0 {
		return pending, time.Time{}
	}
	// 从后往前：later 记下后面要发布的提交涉及的实体，被它们接在后面的留存提交改为立即代发（可以连锁）。
	later := make(map[int64]struct{})
	for i := len(pending) - 1; i >= 0; i-- {
		if !publish[i] {
			for _, commit := range pending[i].Commits {
				if _, found := later[commit.EntityID]; found {
					publish[i] = true
					deferred--
					metrics.IncCounter("remote_entity.outbox.takeover_total", metrics.Labels{"reason": "successor"}, 1)
					break
				}
			}
		}
		if publish[i] {
			for _, commit := range pending[i].Commits {
				later[commit.EntityID] = struct{}{}
			}
		}
	}
	selected := make([]entity.RemoteCommitStatus, 0, len(pending)-deferred)
	var due time.Time
	for i := range pending {
		if publish[i] {
			selected = append(selected, pending[i])
			continue
		}
		if at := pending[i].CreatedAt.Add(grace); due.IsZero() || at.Before(due) {
			due = at
		}
	}
	return selected, due
}

// outboxTakeoverGrace 是代发别的进程提交前的宽限期：配置值，零值与下限都取 FinalizeProjectionTimeout（理由见
// Config.OutboxTakeoverGrace）。
func (m *Manager) outboxTakeoverGrace() time.Duration {
	grace := m.finalizeProjectionTimeout()
	if m.cfg != nil && m.cfg.OutboxTakeoverGrace > grace {
		grace = m.cfg.OutboxTakeoverGrace
	}
	return grace
}

// armOutboxTakeover 在 due（最早一笔留存提交的到期时刻）再唤醒一次发布循环：写者崩溃后可能再没有本地写去唤醒
// 协调者，留存的提交仍要在宽限期后发布。已有更早的计时器时不动它；它触发后的扫描会按那时的结果重新安排。
// 停机后不再安排。
func (m *Manager) armOutboxTakeover(due time.Time) {
	if due.IsZero() || m == nil || m.remote == nil {
		return
	}
	state := m.remote
	now := time.Now
	if state.outboxNow != nil {
		now = state.outboxNow
	}
	state.retryMu.Lock()
	stopping := state.stopping
	state.retryMu.Unlock()
	if stopping {
		return
	}
	state.republishMu.Lock()
	defer state.republishMu.Unlock()
	if state.takeoverTimer != nil && !state.takeoverDue.After(due) {
		return
	}
	if state.takeoverTimer != nil {
		state.takeoverTimer.Stop()
	}
	state.takeoverDue = due
	var timer *time.Timer
	timer = time.AfterFunc(max(due.Sub(now()), 0), func() {
		state.republishMu.Lock()
		if state.takeoverTimer == timer {
			state.takeoverTimer = nil
		}
		state.republishMu.Unlock()
		m.scheduleOutboxRepublish()
	})
	state.takeoverTimer = timer
}
