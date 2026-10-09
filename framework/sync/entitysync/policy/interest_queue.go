package policy

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

var ErrInterestQueueFull = errors.New("policy: interest fact queue is full")

type queuedInterestFact struct {
	id        int64
	condition func() (bool, bool)
	at        spatial.Point
	observer  bool
	// subjectGone：Hide 在事实应用前移走了被观察方（RR-20261006-48），只剩观察者那一半要移动。
	subjectGone bool
	relation    string
	subjects    []int64
}

// ---- Leave / Hide 与排队事实（RR-20261006-48） ----
//
// 排队事实在调用顺序上早于之后的 Leave / Hide，却在下一次政策阶段才应用。旧实现 Leave / Hide 不碰 in.facts：
// 就绪的位置事实在 MoveSubject 处得到 ErrInterestUnknown，被留下并阻塞该 id，错误一路返回到 Flush，Flush 在
// 取 pending 之前失败，之后每一次都一样——这个 Manager 上所有 subject 停发，Stop / Drain 一直报错。
//
// 修法是把 Leave / Hide 的效果写进排队事实，使应用结果与“先应用事实、再 Leave / Hide”逐项相同，不多丢也不多留：
//   - 位置事实：MoveSubject 后 RemoveSubject ≡ RemoveSubject，被观察方那一半作废；MoveObserver 后 RemoveObserver
//     同理。两半都作废才丢弃整条——Hide 只移走被观察方，observer=true 的事实保留观察者的移动。
//   - 关系事实：Set(o, S) 后 Forget(id) ≡ Set(o, S\{id})，从成员里去掉 id；Leave 还 Clear(id)，
//     Set(id, S) 后 Clear(id) ≡ Clear(id)，id 自己作为观察者的关系事实整条丢弃。
// 只按 AOI 实际发生的删除改写（RemoveObserver / RemoveSubject 失败时那一半不动），所以不会丢掉仍然有效的事实。

// supersedeQueuedRelationsLocked 在关系源 Forget(id)（clear 为真时还有 Clear(id)）之后调用。调用方持有 in.mu。
func (in *Interest) supersedeQueuedRelationsLocked(id int64, clear bool) {
	in.rewriteFactsLocked(func(fact *queuedInterestFact) bool {
		if fact.relation == "" {
			return true
		}
		if clear && fact.id == id {
			return false
		}
		fact.subjects = slices.DeleteFunc(fact.subjects, func(subject int64) bool { return subject == id })
		return true
	})
}

// supersedeQueuedMovesLocked 在 AOI 实际移走 id 的被观察方（subject）或观察者（observer）之后调用。调用方持有 in.mu。
func (in *Interest) supersedeQueuedMovesLocked(id int64, subject, observer bool) {
	in.rewriteFactsLocked(func(fact *queuedInterestFact) bool {
		if fact.relation != "" || fact.id != id {
			return true
		}
		fact.subjectGone = fact.subjectGone || subject
		fact.observer = fact.observer && !observer
		return !fact.subjectGone || fact.observer
	})
}

func (in *Interest) rewriteFactsLocked(keep func(*queuedInterestFact) bool) {
	kept := in.facts[:0]
	for _, fact := range in.facts {
		if keep(&fact) {
			kept = append(kept, fact)
		}
	}
	clear(in.facts[len(kept):])
	in.facts = kept
}

// QueueMove 在 Entity 锁内记录位置事实；锁外确认提交后、捕获同步内容前应用。
// observer=true 同时移动观察者，false 只移动被观察实体。失败需由 handler 返回。
func (in *Interest) QueueMove(e entity.IThreadSafeEntity, at spatial.Point, observer bool) error {
	return in.queueFact(e, queuedInterestFact{at: at, observer: observer})
}

// QueueRelation 复制关系成员，避免业务后续修改切片。source 必须已声明。
func (in *Interest) QueueRelation(e entity.IThreadSafeEntity, source string, subjects []int64) error {
	if source == "" {
		return errors.New("policy: relation source required")
	}
	return in.queueFact(e, queuedInterestFact{relation: source, subjects: slices.Clone(subjects)})
}
func (in *Interest) queueFact(e entity.IThreadSafeEntity, fact queuedInterestFact) error {
	if e == nil || e.Base() == nil {
		return errors.New("policy: entity required")
	}
	fact.id = e.ID()
	fact.condition = entity.SyncConditionFor(e)
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if fact.relation != "" && in.relations[fact.relation] == nil {
		return errors.New("policy: unknown relation source")
	}
	if fact.relation == "" {
		if !in.aoi.Bounds().Contains(fact.at) {
			return spatial.ErrInvalidBounds
		}
		if _, ok := in.aoi.subjects[fact.id]; !ok {
			return ErrInterestUnknown
		}
		if fact.observer && in.aoi.observers[fact.id] == nil {
			return ErrInterestUnknown
		}
	}
	if len(in.facts) >= in.maxQueuedFacts {
		return ErrInterestQueueFull
	}
	in.queueActive = true
	in.facts = append(in.facts, fact)
	if ready, _ := fact.condition(); ready {
		in.wake()
	}
	return nil
}
func (in *Interest) applyQueued() error {
	in.mu.Lock()
	if in.closed || !in.queueActive {
		in.mu.Unlock()
		return nil
	}
	blocked := make(map[int64]bool)
	pending := in.facts[:0]
	var failed error
	for _, fact := range in.facts {
		ready, discarded := fact.condition()
		if discarded {
			continue
		}
		if !ready || blocked[fact.id] {
			blocked[fact.id] = true
			pending = append(pending, fact)
			continue
		}
		var err error
		if fact.relation != "" {
			in.relations[fact.relation].Set(fact.id, fact.subjects)
		} else {
			if !fact.subjectGone {
				err = in.aoi.MoveSubject(fact.id, fact.at)
			}
			if err == nil && fact.observer {
				err = in.aoi.MoveObserver(fact.id, fact.at)
			}
		}
		if err != nil {
			// Leave / Hide 已改写失效的事实，这里按理不再失败；万一失败（将来又出现别的移除路径），报告一次后
			// 丢弃，不留下来让之后每一次 Flush 都失败（RR-20261006-48）。同 id 之后的事实仍等到下一轮，保持顺序。
			failed = errors.Join(failed, fmt.Errorf("policy: dropped queued fact for %d: %w", fact.id, err))
			blocked[fact.id] = true
		}
	}
	clear(in.facts[len(pending):])
	in.facts = pending
	in.mu.Unlock()
	// 订阅容量拒绝由 Interest.retry 保留；不能阻止本轮 remove 释放容量。
	in.Apply()
	return failed
}

// queuedPending 告诉 Drain 政策还有没做完的事：排队事实，以及还没停滞的重试 pair（RR-20261006-49）。
func (in *Interest) queuedPending() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed || !in.queueActive {
		return false
	}
	if len(in.facts) > 0 {
		return true
	}
	for _, key := range in.retry {
		if in.refused[key] < retryStallAfter {
			return true
		}
	}
	return false
}

// ---- 重试停滞（RR-20261006-49） ----
//
// 被拒的订阅留在 retry 里，每次 Apply 重说，直到被接受或 pair 被释放（拒绝不是对 pair 的裁决，RR-20260920-06）。
// 旧实现排队模式把整个 retry 计入 queuedPending：一个永远不会被接受的 pair（观察者会话已关闭且不再重开、
// subject 已注销而政策没 Hide……）让 Drain 永不结束，只能等 ctx 超时，kit 停机走不到 Close；排队模式又丢掉
// Refusal，没有任何日志。
//
// 现在同一 pair 连续被拒 retryStallAfter 次即判定为停滞：计数 entitysync_interest_retry_stalled_total、Warn 一次，
// 不再计入 queuedPending。它仍留在 retry 里照旧重说——会话重开、容量释放后照常订阅，计数随成功清零。
// 上界按 Flush 轮次计，默认周期 50ms 下约 500ms：足够覆盖“本轮 remove 释放容量、下一轮接受”这类暂时拒绝。
const retryStallAfter = 10

// noteRefusedLocked 记一次拒绝，到达上界时告警。调用方持有 in.mu。
func (in *Interest) noteRefusedLocked(key pair, err error) {
	if in.refused == nil {
		in.refused = make(map[pair]int)
	}
	in.refused[key]++
	if in.refused[key] != retryStallAfter {
		return
	}
	metrics.IncCounter("entitysync_interest_retry_stalled_total", nil, 1)
	slog.Warn("entitysync/policy: subscription refused repeatedly; still retried but no longer holds Drain",
		"observer", key.observer, "subject", key.subject, "session", in.session(key.observer),
		"attempts", retryStallAfter, "err", err)
}
