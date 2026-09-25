package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/worker"
)

// workers 尚未被 Go 调度时，准入仍应预留其执行额度。这里仅启用准入，
// 不启动消费 goroutine，确定性复现突发抵达先于 worker 运行的时序。
func TestWorkerBudgetAdmitsBurstBeforeWorkersRun(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "fast", true: "slow"}[slow], func(t *testing.T) {
			q := newDispatchQueue("burst", WorkerPoolConfig{1024, 16}, WorkerPoolConfig{1024, 16}, nil, nil)
			q.started = true
			var accepted []*Msg
			defer func() {
				for _, msg := range accepted {
					msg.OnRelease()
				}
			}()
			for i := range 1040 {
				msg := queueMessage(int64(i + 1))
				if err := q.admit(msg, slow); err != nil {
					msg.OnRelease()
					t.Fatalf("burst %d rejected with unused worker capacity: %v", i+1, err)
				}
				accepted = append(accepted, msg)
			}
			extra := queueMessage(2000)
			defer extra.OnRelease()
			if err := q.admit(extra, slow); !errors.Is(err, worker.ErrWorkerQueueFull) {
				t.Fatalf("overload=%v", err)
			}
			fastStats, slowStats, _ := q.stats()
			stats := fastStats
			if slow {
				stats = slowStats
			}
			if stats.QueueLen != 16 {
				t.Fatalf("waiting=%d want 16", stats.QueueLen)
			}
		})
	}
}

func TestWorkerBudgetDoesNotReserveWorkersForBlockedIDs(t *testing.T) {
	q := newDispatchQueue("ordered", WorkerPoolConfig{1, 16}, WorkerPoolConfig{1024, 16}, nil, nil)
	q.started = true
	var accepted []*Msg
	defer func() {
		for _, msg := range accepted {
			msg.OnRelease()
		}
	}()
	for range 17 {
		msg := queueMessage(1)
		if err := q.admit(msg, true); err != nil {
			msg.OnRelease()
			t.Fatal(err)
		}
		accepted = append(accepted, msg)
	}
	blocked := queueMessage(1)
	defer blocked.OnRelease()
	if err := q.admit(blocked, true); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("blocked ID bypassed waiting budget: %v", err)
	}
	// 热 ID 等待已满，独立 ID 仍可使用其余 worker 额度。
	independent := queueMessage(2)
	if err := q.admit(independent, true); err != nil {
		independent.OnRelease()
		t.Fatalf("unused worker rejected independent ID: %v", err)
	}
	accepted = append(accepted, independent)
}

func TestWorkerBudget1024WorkersDrainWithSmallQueue(t *testing.T) {
	entered := make(chan struct{}, 1024)
	release := make(chan struct{})
	q := newDispatchQueue("drain", WorkerPoolConfig{2, 16}, WorkerPoolConfig{1024, 16}, nil, func(*Msg) { entered <- struct{}{}; <-release })
	q.start()
	defer q.stop(context.Background())
	defer close(release)
	for i := range 1024 {
		admitQueue(t, q, true, int64(i+1))
	}
	for range 1024 {
		stagedSignal(t, entered)
	}
	for range 16 {
		admitQueue(t, q, true, 1)
	}
	_, stats, _ := q.stats()
	if stats.QueueLen != 16 {
		t.Fatalf("waiting=%d", stats.QueueLen)
	}
	extra := queueMessage(1)
	if err := q.admit(extra, true); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("limit=%v", err)
	}
	extra.OnRelease()
}
