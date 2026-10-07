package policy

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// RR-20261006-49（F04-2）：排队模式下 Interest 把 retry 里的 pair 计入 queuedPending，Drain 要等它清空才结束。
// 一个永远不会被接受的 pair（观察者会话已关闭且不再重开、subject 已注销而政策没 Hide……）让 queuedPending
// 恒真，Drain 只能等 ctx 超时；kit 停机在 Drain 处返回，走不到 Close。排队模式又丢掉 Refusal，没有任何日志。
// 承诺：同一 pair 连续被拒达到上限后判定为“停滞”，计数（entitysync_interest_retry_stalled_total）并告警一次，
// 不再拖住 Drain；pair 仍留在 retry 里、每次 Apply 照旧重说（拒绝不是对 pair 的裁决，RR-20260920-06），
// 会话重开后照常订阅，不丢合法的兴趣变更。
func TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen(t *testing.T) {
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	in := newInterest(t, m, "team")
	player(t, m, 1)
	player(t, m, 2)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
	mustApply(t, in)
	m.CloseSession(2)

	e := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
	e.SetSyncState(subjectState(t, 2))
	batch := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
	if err := in.QueueRelation(e, "team", []int64{1}); err != nil {
		t.Fatal(err)
	}
	batch.Admit()
	batch.Confirm()
	batch.Release()

	stalledBefore := counterValue("entitysync_interest_retry_stalled_total")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Drain(ctx); err != nil {
		t.Fatalf("Drain with a pair whose session is closed for good: %v (retry=%d)", err, retryLen(in))
	}
	if got := counterValue("entitysync_interest_retry_stalled_total") - stalledBefore; got != 1 {
		t.Fatalf("stalled retry counted %d times, want 1", got)
	}
	if retryLen(in) != 1 {
		t.Fatalf("stalled pair left the retry list: retry=%d", retryLen(in))
	}
	// 再排一轮 Drain 也不再计数（每个 pair 只告警一次）。
	if err := m.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counterValue("entitysync_interest_retry_stalled_total") - stalledBefore; got != 1 {
		t.Fatalf("stalled retry re-counted: %d", got)
	}
	// 会话重开：停滞的 pair 仍被重说，订阅成功。
	if err := m.OpenSession(2); err != nil {
		t.Fatal(err)
	}
	if !subscribed(m, 2, 1) {
		t.Fatal("stalled pair was dropped: reopened session never subscribed")
	}
	if retryLen(in) != 0 {
		t.Fatalf("accepted pair still in retry: %d", retryLen(in))
	}
}

func retryLen(in *Interest) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.retry)
}

func counterValue(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}
