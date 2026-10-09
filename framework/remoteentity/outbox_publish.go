package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
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
		workers = 8
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
