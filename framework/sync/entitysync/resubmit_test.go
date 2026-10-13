// 撤销记录交还政策：只交还政策来源的订阅、再次撤销后回到撤销表且只交还一次、释放通知先于重新提交并带生命期戳。

package entitysync

import (
	"errors"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// 只有经政策来源（NewSubscriptionSourceWithResubmit）订阅的 pair 会在重新登记时交还政策；直接 Manager.Subscribe 与普通
// SubscriptionSource 的订阅没有政策可问，重新登记后不自动恢复。
// 历史：RR-20260926-70。
func TestRetractedSubscriptionsWithoutAPolicyAreNotRestored(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1, 2)
	retracted := labelledSubject(3301, "phantom")
	if err := manager.Register(retracted); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, manager, 1, 3301, entity.SyncProfile{})
	if err := manager.NewSubscriptionSource().Subscribe(2, 3301, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	transport.take(1)
	transport.take(2)
	retracted.Close()
	if !manager.RetractUnloadedSubject(3301) {
		t.Fatal("premise: retract refused")
	}
	mustFlush(t, manager)
	if err := manager.Register(labelledSubject(3301, "mongo")); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	mustFlush(t, manager)
	if got := manager.Subscribers(3301); len(got) != 0 {
		t.Fatalf("subscriptions without a policy came back on their own: %v", got)
	}
}

// resubmitRecorder is a minimal policy: it records what the manager hands back and, like a real policy,
// subscribes again from inside the callback (Flush's policy phase, no manager lock held).
type resubmitRecorder struct {
	source  *SubscriptionSource
	batches [][]RetractedSubscription
	keep    func(RetractedSubscription) bool
}

func newResubmitRecorder(manager *Manager) *resubmitRecorder {
	r := &resubmitRecorder{keep: func(RetractedSubscription) bool { return true }}
	r.source = manager.NewSubscriptionSourceWithResubmit(func(batch []RetractedSubscription) {
		r.batches = append(r.batches, batch)
		for _, subscription := range batch {
			if r.keep(subscription) {
				_ = r.source.Subscribe(subscription.Session, subscription.Subject, entity.SyncProfile{})
			}
		}
	})
	return r
}

func retractWithPolicy(t *testing.T, manager *Manager, recorder *resubmitRecorder, id int64, sessions ...SessionID) *entity.SubjectSyncState {
	t.Helper()
	state := labelledSubject(id, "phantom")
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if err := recorder.source.Subscribe(session, id, entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	mustFlush(t, manager)
	state.Close()
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: retract refused")
	}
	return state
}

// 交还的时机与次数：撤销后、重新登记前不交还；登记后在下一次 Flush 的政策阶段交还一次（同一 Flush 内的重新订阅
// 就被捕获，订阅者收到 create）；之后不再重复。
func TestRetractedPolicySubscriptionsAreHandedBackOnceAfterReRegistration(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1, 2)
	recorder := newResubmitRecorder(manager)
	retractWithPolicy(t, manager, recorder, 3302, 1, 2)
	mustFlush(t, manager) // removes
	mustFlush(t, manager)
	transport.take(1)
	transport.take(2)
	if len(recorder.batches) != 0 {
		t.Fatalf("handed back before the subject was registered again: %v", recorder.batches)
	}
	if err := manager.Register(labelledSubject(3302, "mongo")); err != nil {
		t.Fatal(err)
	}
	if len(recorder.batches) != 0 {
		t.Fatalf("handed back on the Register call path instead of the policy phase: %v", recorder.batches)
	}
	if !manager.policiesPending() {
		t.Fatal("a pending hand-back is not reported to Drain")
	}
	mustFlush(t, manager)
	if len(recorder.batches) != 1 || len(recorder.batches[0]) != 2 {
		t.Fatalf("hand-back batches=%v, want one batch with both sessions", recorder.batches)
	}
	for _, session := range []SessionID{1, 2} {
		if created := oneFrame(t, transport, session); created.objects[3302] != frame.ObjectCreate {
			t.Fatalf("session %d after the resubmit=%v, want a create in the same flush", session, created.objects)
		}
	}
	mustFlush(t, manager)
	if len(recorder.batches) != 1 || manager.policiesPending() {
		t.Fatalf("handed back more than once: %v", recorder.batches)
	}
}

// 来源在缺席期间自己释放了 pair（Unsubscribe 得到 ErrSubjectNotRegistered），那条不再交还；其余照常交还。
// 业务 Unregister 该 ID（退役中）则全部不再交还。
func TestReleasedOrUnregisteredRetractionsAreNotHandedBack(t *testing.T) {
	t.Run("released by its source", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1, 2)
		recorder := newResubmitRecorder(manager)
		retractWithPolicy(t, manager, recorder, 3303, 1, 2)
		mustFlush(t, manager)
		if err := recorder.source.Unsubscribe(1, 3303); !errors.Is(err, ErrSubjectNotRegistered) {
			t.Fatalf("releasing a pair of an absent subject = %v, want ErrSubjectNotRegistered", err)
		}
		if err := manager.Register(labelledSubject(3303, "mongo")); err != nil {
			t.Fatal(err)
		}
		mustFlush(t, manager)
		if len(recorder.batches) != 1 || len(recorder.batches[0]) != 1 || recorder.batches[0][0] != (RetractedSubscription{Session: 2, Subject: 3303}) {
			t.Fatalf("hand-back=%v, want only session 2 (session 1's pair was released)", recorder.batches)
		}
		if got := manager.Subscribers(3303); len(got) != 1 || got[0] != 2 {
			t.Fatalf("subscribers after the hand-back=%v, want [2]", got)
		}
	})
	t.Run("released while the removes are still owed", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1)
		recorder := newResubmitRecorder(manager)
		retractWithPolicy(t, manager, recorder, 3304, 1)
		if err := recorder.source.Unsubscribe(1, 3304); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("releasing a retracted pair while retiring = %v, want ErrSubscriptionNotFound", err)
		}
		mustFlush(t, manager)
		if err := manager.Register(labelledSubject(3304, "mongo")); err != nil {
			t.Fatal(err)
		}
		mustFlush(t, manager)
		if len(recorder.batches) != 0 {
			t.Fatalf("a released pair was handed back: %v", recorder.batches)
		}
	})
	t.Run("business unregister", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1)
		recorder := newResubmitRecorder(manager)
		retractWithPolicy(t, manager, recorder, 3305, 1)
		if err := manager.Unregister(3305); err != nil {
			t.Fatal(err)
		}
		mustFlush(t, manager)
		if queued, err := manager.RegisterAfterRetirement(labelledSubject(3305, "mongo"), nil); err != nil || queued {
			t.Fatalf("(%v, %v)", queued, err)
		}
		mustFlush(t, manager)
		if len(recorder.batches) != 0 {
			t.Fatalf("handed back after the business unregistered the subject: %v", recorder.batches)
		}
	})
	t.Run("queued registration behind the retraction", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1)
		recorder := newResubmitRecorder(manager)
		retractWithPolicy(t, manager, recorder, 3306, 1)
		transport.take(1)                                                      // 撤销前的 create
		if err := manager.Rebind(labelledSubject(3306, "mongo")); err != nil { // kit OnEntityLoaded：排到 remove 之后
			t.Fatal(err)
		}
		mustFlush(t, manager) // remove 交付 → forget → 登记排队的状态
		if removed := oneFrame(t, transport, 1); len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove {
			t.Fatalf("retraction frame=%+v, want ObjectRemove", removed.wire.Objects)
		}
		mustFlush(t, manager)
		if len(recorder.batches) != 1 {
			t.Fatalf("hand-back after the queued registration=%v, want one", recorder.batches)
		}
		if created := oneFrame(t, transport, 1); created.objects[3306] != frame.ObjectCreate {
			t.Fatalf("resubmitted subscription=%v, want a create after the remove", created.objects)
		}
	})
}

// 交还政策的重新提交还没交付（或正在交付、政策的 Subscribe 还没落下），同 ID 的新 subject 又被 RetractUnloadedSubject 撤销：
// 这些记录回到撤销表，下一次登记后再交还一次；同一来源、同一会话只留一条，条数不超过政策持有的 pair；政策在缺席期间释放的照常不交还。
// 历史：RR-20260926-78。

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

// register 放开 m.mu 之后、交还撤销记录之前，新登记的同 ID subject 又被 RetractUnloadedSubject 撤销：记录不交给这个已退役的
// subject，留在撤销表里；第三次登记后恰好交还一次，订阅者收到 create，之后不留记录。窗口由 registeredBeforeHandBack 测试缝
// 确定性进入，不靠 sleep 或放大轮数。
func TestRetractedBetweenRegisterAndHandBackIsHandedBackAfterTheNextRegistration(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1)
	const id = 3501
	recorder := newResubmitRecorder(manager)
	retractWithPolicy(t, manager, recorder, id, 1)
	mustFlush(t, manager) // remove → forget
	mustFlush(t, manager)
	transport.take(1)
	if got := manager.resubmitEntries.Load(); got != 1 {
		t.Fatalf("premise: records after the first retraction=%d, want 1", got)
	}

	second := labelledSubject(id, "second")
	retractedInWindow := false
	manager.registeredBeforeHandBack = func(subjectID int64) {
		manager.registeredBeforeHandBack = nil
		// 重载器在登记放开 m.mu 之后、交还之前又把刚登记的 subject 退回 remove。
		second.Close()
		retractedInWindow = manager.RetractUnloadedSubject(subjectID)
	}
	if err := manager.Register(second); err != nil {
		t.Fatal(err)
	}
	if !retractedInWindow {
		t.Fatal("premise: the retraction between register and hand-back was refused")
	}
	mustFlush(t, manager)
	mustFlush(t, manager)
	if len(recorder.batches) != 0 {
		t.Fatalf("records handed back to a subject retracted before the hand-back: %v", recorder.batches)
	}
	if got := manager.Subscribers(id); len(got) != 0 {
		t.Fatalf("premise: the retracted subject still has subscribers %v", got)
	}
	if got := manager.resubmitEntries.Load(); got != 1 {
		t.Fatalf("records after the retraction in the window=%d, want the one record kept for the next registration", got)
	}
	transport.take(1)

	if err := manager.Register(labelledSubject(id, "third")); err != nil {
		t.Fatalf("third register: %v", err)
	}
	mustFlush(t, manager)
	if len(recorder.batches) != 1 || len(recorder.batches[0]) != 1 || recorder.batches[0][0] != (RetractedSubscription{Session: 1, Subject: id}) {
		t.Fatalf("hand-back after the third registration=%v, want session 1 exactly once", recorder.batches)
	}
	if got := manager.Subscribers(id); len(got) != 1 || got[0] != 1 {
		t.Fatalf("subscribers after the hand-back=%v, want [1]", got)
	}
	if created := oneFrame(t, transport, 1); created.objects[id] != frame.ObjectCreate {
		t.Fatalf("session 1 after the hand-back=%v, want a create", created.objects)
	}
	if manager.resubmitEntries.Load() != 0 || manager.policiesPending() {
		t.Fatalf("records left behind after delivery: %d", manager.resubmitEntries.Load())
	}
}

// 带 Released 回调的来源（NewSubscriptionSourceWithHooks）在框架替它丢掉订阅时得到通知：CloseSession、传输拒绝帧后 Manager
// 关闭会话、业务 Unregister；撤销后尚待重新提交的记录同样通知，RetractUnloadedSubject 本身不算释放。通知在下一次 Flush 的
// 政策阶段、重新提交之前交付，不在调用方路径上回调。Holds 只对当前这一次打开的会话、未退役的 subject、来源仍在的订阅为真。
// 历史：RR-20260926-79。

type releaseRecorder struct {
	source   *SubscriptionSource
	released []ReleasedSubscription
	order    []string
}

func newReleaseRecorder(manager *Manager) *releaseRecorder {
	r := &releaseRecorder{}
	r.source = manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{
		Resubmit: func(batch []RetractedSubscription) {
			r.order = append(r.order, "resubmit")
		},
		Released: func(batch []ReleasedSubscription) {
			r.order = append(r.order, "released")
			r.released = append(r.released, batch...)
		},
	})
	return r
}

// take 返回已收到的 (会话, subject)，按会话、subject 排序；戳另有回归，这里清零后比较。
func (r *releaseRecorder) take() []ReleasedSubscription {
	out := r.released
	r.released = nil
	for i := range out {
		out[i].Stamp = 0
	}
	slices.SortFunc(out, func(a, b ReleasedSubscription) int {
		if a.Session != b.Session {
			return int(a.Session - b.Session)
		}
		return int(a.Subject - b.Subject)
	})
	return out
}

func TestReleasedHookReportsWhatTheFrameworkDropped(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1, 2, 3)
	recorder := newReleaseRecorder(manager)
	plain := manager.NewSubscriptionSource()
	states := make(map[int64]*entity.SubjectSyncState)
	for _, id := range []int64{3501, 3502, 3503, 3504} {
		states[id] = labelledSubject(id, "live")
		if err := manager.Register(states[id]); err != nil {
			t.Fatal(err)
		}
	}
	retracted := labelledSubject(3505, "phantom")
	if err := manager.Register(retracted); err != nil {
		t.Fatal(err)
	}
	for _, pair := range []ReleasedSubscription{{Session: 1, Subject: 3501}, {Session: 1, Subject: 3505}, {Session: 2, Subject: 3502}, {Session: 3, Subject: 3503}} {
		if err := recorder.source.Subscribe(pair.Session, pair.Subject, entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := plain.Subscribe(2, 3504, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if !recorder.source.Holds(1, 3501) || recorder.source.Holds(2, 3504) || plain.Holds(1, 3501) {
		t.Fatal("Holds does not follow the source's own subscriptions")
	}

	// RR-59 撤销不是释放。
	retracted.Close()
	if !manager.RetractUnloadedSubject(3505) {
		t.Fatal("premise: retract refused")
	}
	mustFlush(t, manager)
	mustFlush(t, manager)
	if got := recorder.take(); len(got) != 0 {
		t.Fatalf("a retraction was reported as a release: %v", got)
	}
	if recorder.source.Holds(1, 3505) {
		t.Fatal("Holds counts a retracted pair")
	}

	// 会话关闭：它的订阅与缺席 subject 的撤销记录都通知；不在 CloseSession 的调用路径上回调。
	manager.CloseSession(1)
	if got := recorder.take(); len(got) != 0 {
		t.Fatalf("released on the CloseSession call path instead of the policy phase: %v", got)
	}
	if !manager.policiesPending() {
		t.Fatal("a pending release is not reported to Drain")
	}
	mustFlush(t, manager)
	if got := recorder.take(); !slices.Equal(got, []ReleasedSubscription{{Session: 1, Subject: 3501}, {Session: 1, Subject: 3505}}) {
		t.Fatalf("released after closing session 1 = %v, want its live subscription and its retracted record", got)
	}
	// RR-70 的撤销记录不因会话关闭而删除（政策决定是否释放）。
	if manager.resubmitEntries.Load() != 1 {
		t.Fatalf("closing the session changed the RR-70 records: %d", manager.resubmitEntries.Load())
	}

	// 业务 Unregister。
	if err := manager.Unregister(3502); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if got := recorder.take(); !slices.Equal(got, []ReleasedSubscription{{Session: 2, Subject: 3502}}) {
		t.Fatalf("released after unregistering 3502 = %v", got)
	}

	// 传输拒绝帧后 Manager 关闭会话。
	transport.breakSession(3, errors.New("peer gone"))
	states[3503].MarkDirty(1)
	mustFlush(t, manager) // 帧被拒绝 → loseSession；通知在下一次政策阶段交付
	if got := recorder.take(); len(got) != 0 {
		t.Fatalf("released inside the failing flush before its policy phase could run: %v", got)
	}
	mustFlush(t, manager)
	if got := recorder.take(); !slices.Equal(got, []ReleasedSubscription{{Session: 3, Subject: 3503}}) {
		t.Fatalf("released after the transport refused session 3 = %v", got)
	}
}

// 同一政策阶段里释放先于重新提交：会话关闭时仍排队的重新提交，政策先得知会话已关、再收到交还。
func TestReleasesAreDeliveredBeforeResubmits(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1)
	recorder := newReleaseRecorder(manager)
	state := labelledSubject(3506, "phantom")
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	if err := recorder.source.Subscribe(1, 3506, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	state.Close()
	if !manager.RetractUnloadedSubject(3506) {
		t.Fatal("premise: retract refused")
	}
	mustFlush(t, manager)
	if err := manager.Register(labelledSubject(3506, "mongo")); err != nil {
		t.Fatal(err)
	}
	manager.CloseSession(1)
	recorder.order = nil
	mustFlush(t, manager)
	if !slices.Equal(recorder.order, []string{"released", "resubmit"}) {
		t.Fatalf("policy phase order=%v, want the release before the resubmit", recorder.order)
	}
}

// 释放通知带戳（ReleasedSubscription.Stamp），与 SubscribeStamped 同一时钟：这一次会话打开 / 这一次登记里做的订阅，戳都小于它的
// 释放通知；通知送达之前在重开的会话 / 重新登记的 subject 上重新做的订阅，戳都大于它——政策据此只删除通知所指那一次生命期里的簿记。
// 历史：RR-20260926-85。
func TestReleaseStampsSeparateTheReleasedLifetimeFromTheNextOne(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1)
	recorder := newReleaseRecorder(manager)
	stamped := func(subject int64) SubscriptionStamp {
		t.Helper()
		stamp, err := recorder.source.SubscribeStamped(1, subject, entity.SyncProfile{})
		if err != nil || stamp == 0 {
			t.Fatalf("SubscribeStamped(1, %d) = %d, %v", subject, stamp, err)
		}
		return stamp
	}
	released := func() ReleasedSubscription {
		t.Helper()
		mustFlush(t, manager)
		got := recorder.released
		recorder.released = nil
		if len(got) != 1 {
			t.Fatalf("released = %v, want one notification", got)
		}
		return got[0]
	}

	// 会话关闭后重开：通知落在两次打开之间。同视图的重复订阅也拿到新的戳。
	if err := manager.Register(labelledSubject(3601, "live")); err != nil {
		t.Fatal(err)
	}
	first := stamped(3601)
	if again := stamped(3601); again <= first {
		t.Fatalf("a repeated subscription did not get a later stamp: %d after %d", again, first)
	}
	before := stamped(3601)
	mustFlush(t, manager)
	manager.CloseSession(1)
	if err := manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	after := stamped(3601)
	if got := released(); got.Session != 1 || got.Subject != 3601 || got.Stamp <= before || got.Stamp >= after {
		t.Fatalf("session release %+v, want stamp between the closed lifetime's %d and the reopened one's %d", got, before, after)
	}

	// 业务注销后同 ID 重新登记：通知落在两次登记之间（未交付的 subject 在 Unregister 时立刻被忘掉）。
	if err := manager.Register(labelledSubject(3602, "live")); err != nil {
		t.Fatal(err)
	}
	before = stamped(3602)
	if err := manager.Unregister(3602); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(labelledSubject(3602, "again")); err != nil {
		t.Fatalf("premise: 3602 was not forgotten at once: %v", err)
	}
	after = stamped(3602)
	if got := released(); got.Session != 1 || got.Subject != 3602 || got.Stamp <= before || got.Stamp >= after {
		t.Fatalf("unregister release %+v, want stamp between the first registration's %d and the next one's %d", got, before, after)
	}
}
