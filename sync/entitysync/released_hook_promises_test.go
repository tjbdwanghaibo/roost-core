package entitysync

import (
	"errors"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-79：带 Released 回调的来源（NewSubscriptionSourceWithHooks）在框架替它丢掉订阅时得到通知：CloseSession、
// 传输拒绝帧后 Manager 关闭会话、业务 Unregister；RR-59 撤销后尚待重新提交的记录同样通知。RR-59 的 RetractUnloadedSubject
// 不算释放。通知在下一次 Flush 的政策阶段、重新提交之前交付，不在调用方路径上回调。Holds 只对当前这一次打开的会话、
// 未退役的 subject、来源仍在的订阅为真。

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

// take 返回已收到的 (会话, subject)，按会话、subject 排序；戳（RR-20260926-85）另有回归，这里清零后比较。
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
