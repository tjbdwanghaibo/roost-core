package entitysync

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// RR-20260926-78：RR-70 交还政策的重新提交还没交付（或正在交付、政策的 Subscribe 还没落下），同 ID 的新 subject 就又被
// RetractUnloadedSubject 撤销。承诺：这些记录回到撤销表，下一次登记后再交还一次；同一来源、同一会话只留一条，条数
// 不超过政策持有的 pair；政策在缺席期间释放的照常不交还。旧行为：再次撤销只记当时存在的订阅，排队项留在队列、交付给
// 已退役 / 已忘掉的 subject，政策的 Subscribe 被拒绝，记录就此消失；同一 pair 既在队列又被重新订阅时交还两条。

// 政策的回调里、Subscribe 落下之前 subject 被再次撤销（交付中）：取出的项复制回撤销表，第三次登记后再交还。
func TestResubmitInterruptedByASecondRetractionIsHandedBackAgain(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1)
	const id = 3401
	var (
		current   *entity.SubjectSyncState
		batches   [][]RetractedSubscription
		refusals  []error
		source    *SubscriptionSource
		interrupt = true
	)
	source = manager.NewSubscriptionSourceWithResubmit(func(batch []RetractedSubscription) {
		batches = append(batches, batch)
		if interrupt {
			// 重载器在政策阶段并发撤销了刚登记的 subject：这里按事件顺序确定性地放在 Subscribe 之前。
			interrupt = false
			current.Close()
			if !manager.RetractUnloadedSubject(id) {
				t.Error("premise: the retraction during delivery was refused")
			}
		}
		for _, subscription := range batch {
			if err := source.Subscribe(subscription.Session, subscription.Subject, entity.SyncProfile{}); err != nil {
				refusals = append(refusals, err)
			}
		}
	})
	current = labelledSubject(id, "phantom")
	if err := manager.Register(current); err != nil {
		t.Fatal(err)
	}
	if err := source.Subscribe(1, id, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	transport.take(1)
	current.Close()
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: first retraction refused")
	}
	mustFlush(t, manager) // remove → forget
	transport.take(1)

	current = labelledSubject(id, "mongo")
	if err := manager.Register(current); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager) // 交付被打断：Subscribe 落在第二次撤销之后
	if len(batches) != 1 || len(refusals) != 1 || !(errors.Is(refusals[0], ErrSubjectNotRegistered) || errors.Is(refusals[0], ErrSubjectRetiring)) {
		t.Fatalf("premise: batches=%v refusals=%v, want one interrupted delivery refused as not registered / retiring", batches, refusals)
	}
	if got := manager.Subscribers(id); len(got) != 0 {
		t.Fatalf("premise: the second retraction left subscribers %v", got)
	}

	if err := manager.Register(labelledSubject(id, "mongo-again")); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if len(batches) != 2 || len(batches[1]) != 1 || batches[1][0] != (RetractedSubscription{Session: 1, Subject: id}) {
		t.Fatalf("hand-back after the third registration=%v, want the interrupted pair handed back once more", batches)
	}
	if created := oneFrame(t, transport, 1); created.objects[id] != frame.ObjectCreate {
		t.Fatalf("session 1 after the second hand-back=%v, want a create", created.objects)
	}
	if manager.resubmitEntries.Load() != 0 || manager.policiesPending() {
		t.Fatalf("records left behind after delivery: %d", manager.resubmitEntries.Load())
	}
}

// 排队未交付时再次撤销：记录回到撤销表、交付不落在缺席的 subject 上；政策在此期间自己也说过的同一 pair 只留一条。
func TestQueuedResubmitReturnsToTheRetractionTableOnce(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1, 2)
	recorder := newResubmitRecorder(manager)
	retractWithPolicy(t, manager, recorder, 3402, 1, 2)
	mustFlush(t, manager) // removes
	mustFlush(t, manager)

	second := labelledSubject(3402, "mongo")
	if err := manager.Register(second); err != nil {
		t.Fatal(err)
	}
	// 政策自己（例如 Apply 的新事件）在交付前已重新说了会话 1 的 pair：它既在队列里，也是当前订阅。
	if err := recorder.source.Subscribe(1, 3402, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	second.Close()
	if !manager.RetractUnloadedSubject(3402) {
		t.Fatal("premise: second retraction refused")
	}
	if got := manager.Subscribers(3402); len(got) != 0 {
		t.Fatalf("premise: nothing held the object, the subject should be forgotten at once: %v", got)
	}
	mustFlush(t, manager)
	if len(recorder.batches) != 0 {
		t.Fatalf("handed back to an absent subject: %v", recorder.batches)
	}
	if got := manager.resubmitEntries.Load(); got != 2 {
		t.Fatalf("records after the second retraction=%d, want 2 (one per pair the policy holds)", got)
	}

	// 缺席期间政策释放会话 2 的 pair：回到撤销表的记录同样可以释放。
	if err := recorder.source.Unsubscribe(2, 3402); !errors.Is(err, ErrSubjectNotRegistered) {
		t.Fatalf("releasing a pair of an absent subject = %v, want ErrSubjectNotRegistered", err)
	}
	if err := manager.Register(labelledSubject(3402, "mongo-again")); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if len(recorder.batches) != 1 || len(recorder.batches[0]) != 1 || recorder.batches[0][0] != (RetractedSubscription{Session: 1, Subject: 3402}) {
		t.Fatalf("hand-back=%v, want exactly session 1 once", recorder.batches)
	}
	if got := manager.Subscribers(3402); len(got) != 1 || got[0] != 1 {
		t.Fatalf("subscribers=%v, want [1]", got)
	}
	if manager.resubmitEntries.Load() != 0 {
		t.Fatalf("records left behind: %d", manager.resubmitEntries.Load())
	}
}
