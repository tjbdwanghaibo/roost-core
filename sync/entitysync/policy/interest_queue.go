package policy

import (
	"errors"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/spatial"
)

var ErrInterestQueueFull = errors.New("policy: interest fact queue is full")

type queuedInterestFact struct {
	id        int64
	condition func() (bool, bool)
	at        spatial.Point
	observer  bool
	relation  string
	subjects  []int64
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
			err = in.aoi.MoveSubject(fact.id, fact.at)
			if err == nil && fact.observer {
				err = in.aoi.MoveObserver(fact.id, fact.at)
			}
		}
		if err != nil {
			failed = errors.Join(failed, err)
			blocked[fact.id] = true
			pending = append(pending, fact)
		}
	}
	clear(in.facts[len(pending):])
	in.facts = pending
	in.mu.Unlock()
	// 订阅容量拒绝由 Interest.retry 保留；不能阻止本轮 remove 释放容量。
	in.Apply()
	return failed
}

func (in *Interest) queuedPending() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return !in.closed && in.queueActive && (len(in.facts) > 0 || len(in.retry) > 0)
}
