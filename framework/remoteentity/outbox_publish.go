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

// publishOutboxPage 由持有outboxSerial的唯一协调者调用；一页最多256项。
// 先按全部Entity建立前序依赖，再只把就绪事务交给有界worker；等待前序的事务
// 不占worker，因此热点链不能堵住同页无关事务。跨页先排空，仍保留原扫描顺序。
// 失败保留Applied，后继等本次尝试结束再尝试，沿用原串行循环的失败语义。
func (m *Manager) publishOutboxPage(ctx context.Context, pending []entity.RemoteCommitStatus) error {
	workers := m.cfg.OutboxPublishWorkers
	if workers <= 0 {
		workers = 32
	}
	workers = min(workers, 64, len(pending))
	if workers == 0 {
		return ctx.Err()
	}
	publications := make([]outboxPublication, len(pending))
	lastEntity := make(map[int64]int)
	lastTransaction := make(map[entity.RemoteTransactionID]int)
	ready := make([]int, 0, len(pending))
	for i, status := range pending {
		publications[i].status = status
		predecessors := make(map[int]struct{})
		if previous, found := lastTransaction[status.TransactionID]; found {
			predecessors[previous] = struct{}{}
		}
		lastTransaction[status.TransactionID] = i
		for _, commit := range status.Commits {
			if previous, found := lastEntity[commit.EntityID]; found && previous != i {
				predecessors[previous] = struct{}{}
			}
			lastEntity[commit.EntityID] = i
		}
		for previous := range predecessors {
			publications[previous].dependents = append(publications[previous].dependents, i)
		}
		publications[i].waiting = len(predecessors)
		if len(predecessors) == 0 {
			ready = append(ready, i)
		}
	}
	jobs := make(chan int, workers)
	results := make(chan outboxPublicationResult, workers)
	var running sync.WaitGroup
	for range workers {
		running.Go(func() {
			for index := range jobs {
				status := publications[index].status
				err := ctx.Err()
				if err == nil {
					err = m.publishAppliedRemoteTransaction(ctx, status)
				}
				results <- outboxPublicationResult{index: index, err: err}
			}
		})
	}
	defer func() {
		close(jobs)
		running.Wait()
	}()
	var failures error
	inFlight := 0
	for len(ready) > 0 || inFlight > 0 {
		for len(ready) > 0 && inFlight < workers && ctx.Err() == nil {
			jobs <- ready[0]
			ready = ready[1:]
			inFlight++
		}
		if inFlight == 0 {
			break // 取消后不再派发；已派发任务必须全部退出才交还生命周期。
		}
		result := <-results
		inFlight--
		item := &publications[result.index]
		if result.err != nil {
			status := item.status
			m.completeRemoteTransaction(status.TransactionID, entity.RemoteCommitStatus{TransactionID: status.TransactionID, State: entity.RemoteCommitIndeterminate, Receipts: status.Receipts, Cause: result.err.Error()})
			failures = errors.Join(failures, fmt.Errorf("remote_entity: recover transaction %s: %w", status.TransactionID, result.err))
		}
		for _, next := range item.dependents {
			publications[next].waiting--
			if publications[next].waiting == 0 {
				ready = append(ready, next)
			}
		}
	}
	return errors.Join(failures, ctx.Err())
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
