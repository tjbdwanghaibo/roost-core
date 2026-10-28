package saga

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// terminal_watch.go 是 WaitTerminal 的终态观察者（设计 §2.1、§7.3；RV-20261028-02 与原型稿 §12b 的约束）：
//
//   - 每条流一个只要新消息（from = 0）的有序消费者，读本流各分区的 ev.done 事件；只读已落库的事件（被期望序号拒掉的发布
//     不会出现在流里），不从流头回放历史，所以没有原型 Client 的 latest 表无界增长问题。本进程自己提交的终态事件另由分区
//     在提交后直接交给观察者（更快；与消费者送来的同一条按版本去重）。
//   - 同一 ID 的每次 WaitTerminal 都登记自己的等待者（不覆盖），被观察到的终态按版本择优交给该 ID 的全部等待者；等待者登记后
//     先 Get 一次：已是终态直接返回，否则只接受版本大于此时已知版本的终态（重开之前的旧终态不会提前唤醒）。
//   - 拒绝（选项 R-A）不是终态、不进日志，只经判定回复给触发它的那次尝试，所以拒绝不会唤醒同 ID 的别的调用。

// terminalWatch 管理等待者；observe 由消费者 goroutine 与分区循环调用，register / 等待由调用方 goroutine 调用。
type terminalWatch struct {
	mu      sync.Mutex
	waiters map[string][]*terminalWaiter
}

// terminalWaiter 是一次 WaitTerminal 的登记：best 是观察到的版本最大的终态。
type terminalWaiter struct {
	mu     sync.Mutex
	best   *Record
	signal chan struct{}
}

func newTerminalWatch() *terminalWatch {
	return &terminalWatch{waiters: map[string][]*terminalWaiter{}}
}

// register 为 id 登记一个等待者，返回它与撤销函数（WaitTerminal 返回前调用）。
func (w *terminalWatch) register(id string) (*terminalWaiter, func()) {
	waiter := &terminalWaiter{signal: make(chan struct{}, 1)}
	w.mu.Lock()
	w.waiters[id] = append(w.waiters[id], waiter)
	w.mu.Unlock()
	return waiter, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		list := w.waiters[id]
		for i, other := range list {
			if other == waiter {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(w.waiters, id)
		} else {
			w.waiters[id] = list
		}
	}
}

// observe 把一条终态记录交给该 ID 的全部等待者。
func (w *terminalWatch) observe(record Record) {
	if !record.Status.Terminal() {
		return
	}
	w.mu.Lock()
	list := append([]*terminalWaiter(nil), w.waiters[record.ID]...)
	w.mu.Unlock()
	for _, waiter := range list {
		waiter.offer(record)
	}
}

// observeCommitted 是分区提交之后的本地快路：这一轮的终态事件。
func (w *terminalWatch) observeCommitted(events []logEvent) {
	for i := range events {
		w.observe(events[i].Saga.Record)
	}
}

func (waiter *terminalWaiter) offer(record Record) {
	waiter.mu.Lock()
	if waiter.best == nil || record.Version > waiter.best.Version {
		clone := record.Clone()
		waiter.best = &clone
	}
	waiter.mu.Unlock()
	select {
	case waiter.signal <- struct{}{}:
	default:
	}
}

// newerThan 返回版本大于 known 的最佳终态（没有为 nil）。
func (waiter *terminalWaiter) newerThan(known uint64) *Record {
	waiter.mu.Lock()
	defer waiter.mu.Unlock()
	if waiter.best != nil && waiter.best.Version > known {
		record := waiter.best.Clone()
		return &record
	}
	return nil
}

// WaitTerminal 等 id 的下一个终态（Completed / Compensated / Failed / ManualRequired）：已是终态立即返回当前记录；否则返回之后
// 被观察到的、版本大于调用时已知版本的终态。同一 ID 的多个等待者都会被唤醒。ctx 结束返回 ctx 错误；不改变 saga 本身。
func (e *JetStreamEngine) WaitTerminal(ctx context.Context, id string) (Record, error) {
	if !validSubjectToken(id, 128) {
		return Record{}, ErrInvalidRecord
	}
	if !e.ready.Load() {
		return Record{}, ErrClosed // 终态观察者随 Start 建立
	}
	waiter, cancel := e.watch.register(id)
	defer cancel()
	var known uint64
	for {
		record, err := e.Get(ctx, id)
		if err == nil {
			if record.Status.Terminal() {
				return record, nil
			}
			known = record.Version
			break
		}
		if errors.Is(err, ErrNotFound) {
			break
		}
		if ctx.Err() != nil {
			return Record{}, ctx.Err()
		}
		// 所有者暂时不可用：过一会再问（这期间观察到的终态不知道是否比当前状态新，不采用）。
		select {
		case <-ctx.Done():
			return Record{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	for {
		if record := waiter.newerThan(known); record != nil {
			return *record, nil
		}
		select {
		case <-ctx.Done():
			return Record{}, ctx.Err()
		case <-waiter.signal:
		}
	}
}

// followTerminals 为每条流建一个只要新消息的有序消费者，读本流各分区的终态事件交给观察者。
func (e *JetStreamEngine) followTerminals(ctx context.Context) ([]fnats.LogFollow, error) {
	var follows []fnats.LogFollow
	for g, stream := range e.layout.Streams() {
		parts := e.layout.PartitionsOfGroup(g)
		filters := make([]string, len(parts))
		for i, k := range parts {
			filters[i] = e.layout.DoneEventSubject(k)
		}
		follow, err := e.log.Follow(ctx, stream, filters, 0, func(msg fnats.LogMsg) {
			k, ok := e.partitionOfSubject(msg.Subject)
			if !ok {
				return
			}
			ev, err := decodeEvent(msg.Data, e.layout, k)
			if err != nil {
				return
			}
			e.watch.observe(ev.Saga.Record)
		})
		if err != nil {
			for _, f := range follows {
				_ = f.Stop(context.Background())
			}
			return nil, err
		}
		follows = append(follows, follow)
	}
	return follows, nil
}

// partitionOfSubject 从 <prefix>.<k>.… 取出分区号。
func (e *JetStreamEngine) partitionOfSubject(subject string) (int, bool) {
	rest, ok := strings.CutPrefix(subject, e.layout.Prefix+".")
	if !ok {
		return 0, false
	}
	token, _, _ := strings.Cut(rest, ".")
	k, err := strconv.Atoi(token)
	if err != nil || k < 0 || k >= e.layout.Partitions {
		return 0, false
	}
	return k, true
}
