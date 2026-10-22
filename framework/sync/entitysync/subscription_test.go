// 订阅：索引跟随所有移除路径、多来源选 profile 与独立释放、已交付对象的移除、部分重试从最后准入帧继续。

package entitysync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

func assertSubscriptionIndex(t *testing.T, m *Manager) {
	t.Helper()
	// 只在操作完成后核对两侧，测试本身不持 Manager 锁后获取 subject 锁。
	for sid, sess := range m.sessions {
		for id, subj := range sess.lifetime.subjects {
			sub := subj.subscribers[sid]
			if m.subjects[id] != subj || sub == nil || sub.lifetime != sess.lifetime {
				t.Fatalf("stale reverse edge sid=%d entity=%d", sid, id)
			}
		}
	}
	for id, subj := range m.subjects {
		for sid, sub := range subj.subscribers {
			sess := m.sessions[sid]
			if sess == nil || sess.lifetime != sub.lifetime || sess.lifetime.subjects[id] != subj {
				t.Fatalf("missing reverse edge sid=%d entity=%d", sid, id)
			}
		}
	}
}

func TestSubscriptionIndexTracksAllRemovalPaths(t *testing.T) {
	for _, action := range []string{"unsubscribe_pending", "unsubscribe_live", "unregister_pending", "unregister_live", "hold_leaving", "close", "transport_loss"} {
		t.Run(action, func(t *testing.T) {
			tr := newRecordingTransport()
			m := newTestManager(t, tr, ManagerConfig{})
			packs := 0
			if err := m.Register(testSubject(t, 7301, &packs)); err != nil {
				t.Fatal(err)
			}
			open(t, m, 1)
			mustSubscribe(t, m, 1, 7301, entity.SyncProfile{})
			other := m.NewSubscriptionSource()
			if err := other.Subscribe(1, 7301, entity.SyncProfile{Key: "other"}); err != nil {
				t.Fatal(err)
			}
			if err := other.Unsubscribe(1, 7301); err != nil {
				t.Fatal(err)
			}
			assertSubscriptionIndex(t, m)
			if action != "unsubscribe_pending" && action != "unregister_pending" {
				mustFlush(t, m)
			}
			lifetime := m.session(1).lifetime
			switch action {
			case "unsubscribe_pending", "unsubscribe_live", "hold_leaving":
				if err := m.Unsubscribe(1, 7301); err != nil {
					t.Fatal(err)
				}
				if action == "hold_leaving" {
					if err := m.HoldSession(1); err != nil {
						t.Fatal(err)
					}
				}
			case "unregister_pending", "unregister_live":
				if err := m.Unregister(7301); err != nil {
					t.Fatal(err)
				}
			case "close":
				m.CloseSession(1)
			case "transport_loss":
				m.loseSession(m.session(1), errors.New("lost"))
			}
			mustFlush(t, m)
			assertSubscriptionIndex(t, m)
			if len(lifetime.subjects) != 0 {
				t.Fatalf("retained edges: %v", lifetime.subjects)
			}
		})
	}
}

func TestSessionRecoveryDoesNotLockUnsubscribedSubjects(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{})
	packs := 0
	for _, id := range []int64{7401, 7402} {
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 7401, entity.SyncProfile{})
	unrelated := m.subject(7402)
	unrelated.mu.Lock()
	done := make(chan error, 1)
	go func() {
		if err := m.HoldSession(1); err != nil {
			done <- err
			return
		}
		if err := m.ReadySession(1); err != nil {
			done <- err
			return
		}
		m.CloseSession(1)
		done <- nil
	}()
	select {
	case err := <-done:
		unrelated.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		unrelated.mu.Unlock()
		<-done
		t.Fatal("session operations waited for an unrelated entity")
	}
	assertSubscriptionIndex(t, m)
}

func TestSubscriptionIndexConcurrentLifecycle(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{})
	packs := 0
	if err := m.Register(testSubject(t, 7501, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	var group sync.WaitGroup
	group.Go(func() {
		for range 200 {
			_ = m.Subscribe(1, 7501, entity.SyncProfile{})
			_ = m.Unsubscribe(1, 7501)
		}
	})
	group.Go(func() {
		for range 200 {
			_ = m.HoldSession(1)
			_ = m.ReadySession(1)
		}
	})
	group.Go(func() {
		for range 200 {
			m.CloseSession(1)
			_ = m.OpenSession(1)
		}
	})
	group.Wait()
	// 最终一轮 Close 必须清掉所有完成准入的订阅，不依赖 Flush 帮忙扫全场。
	m.CloseSession(1)
	assertSubscriptionIndex(t, m)
	if len(m.Subscribers(7501)) != 0 {
		t.Fatal("closed session retained subscriptions")
	}
}

type heldCloseTransport struct {
	*recordingTransport
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*heldCloseTransport) SessionOpened(SessionID) error { return nil }
func (tr *heldCloseTransport) SessionClosed(SessionID) {
	tr.once.Do(func() { close(tr.entered); <-tr.release })
}

func TestFlushDoesNotSendOldSubscriptionToReopenedSession(t *testing.T) {
	tr := &heldCloseTransport{recordingTransport: newRecordingTransport(), entered: make(chan struct{}), release: make(chan struct{})}
	m := newTestManager(t, tr, ManagerConfig{})
	packs := 0
	if err := m.Register(testSubject(t, 7601, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 7601, entity.SyncProfile{})
	old := m.session(1).lifetime
	done := make(chan struct{})
	go func() { m.CloseSession(1); close(done) }()
	<-tr.entered
	// Close 已移除会话，但外部生命周期回调尚未返回，旧订阅还在 subject 上。
	open(t, m, 1)
	if err := m.Flush(context.Background()); err != nil {
		t.Error(err)
	}
	frames := tr.take(1)
	close(tr.release)
	<-done
	if len(frames) != 0 {
		t.Fatalf("reopened session received %d frames without subscribing", len(frames))
	}
	if len(old.subjects) != 0 {
		t.Fatal("old lifetime retained subscription")
	}
	assertSubscriptionIndex(t, m)
}

// 等待新快照不等于客户端没有旧对象：换 profile、撤回退订都会进入这个状态。
// 只要旧对象已交付，最终退订或退役就必须向客户端发送对应 ObjectRef 的 remove。
func TestPendingSnapshotStillRemovesAnAlreadyDeliveredObject(t *testing.T) {
	for _, transition := range []string{"profile_change", "resubscribe"} {
		for _, removal := range []string{"unsubscribe", "unregister"} {
			t.Run(transition+"/"+removal, func(t *testing.T) {
				transport := newRecordingTransport()
				manager := newTestManager(t, transport, ManagerConfig{})
				packs := 0
				state := testSubject(t, 1101, &packs)
				if err := manager.Register(state); err != nil {
					t.Fatal(err)
				}
				open(t, manager, 1, 2)
				mustSubscribe(t, manager, 1, 1101, entity.SyncProfile{})
				mustFlush(t, manager)
				created := oneFrame(t, transport, 1)
				ref := created.wire.Objects[0].Ref

				profile := entity.SyncProfile{Key: "far", LOD: 1}
				if transition == "resubscribe" {
					if err := manager.Unsubscribe(1, 1101); err != nil {
						t.Fatal(err)
					}
					profile = entity.SyncProfile{}
				}
				mustSubscribe(t, manager, 1, 1101, profile)
				// 会话 2 同样等待快照，但从未收到对象，不应收到删除通知。
				mustSubscribe(t, manager, 2, 1101, profile)
				if removal == "unregister" {
					if err := manager.Unregister(1101); err != nil {
						t.Fatal(err)
					}
				} else {
					for _, sid := range []SessionID{1, 2} {
						if err := manager.Unsubscribe(sid, 1101); err != nil {
							t.Fatal(err)
						}
					}
				}
				mustFlush(t, manager)
				removed := oneFrame(t, transport, 1)
				if len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove || removed.wire.Objects[0].Ref != ref {
					t.Fatalf("remove must name the delivered object %v, got %+v", ref, removed.wire.Objects)
				}
				if got := transport.take(2); len(got) != 0 {
					t.Fatalf("a receiver with no object got %d frames", len(got))
				}
				if got := manager.Subscribers(1101); len(got) != 0 {
					t.Fatalf("subscriptions remain after removal: %v", got)
				}
				if removal == "unregister" && manager.Stats().Subjects != 0 {
					t.Fatal("the retired subject is still registered")
				}
			})
		}
	}
}

func TestSourcesSelectOneProfileAndReleaseIndependently(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 5301, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	near, far := manager.NewSubscriptionSource(), manager.NewSubscriptionSource()
	bind := func(source *SubscriptionSource, p entity.SyncProfile) {
		t.Helper()
		if err := source.Subscribe(1, 5301, p); err != nil {
			t.Fatal(err)
		}
	}
	bind(far, entity.SyncProfile{Key: "far", LOD: 3})
	for range 3 {
		bind(near, entity.SyncProfile{Key: "near", LOD: 1})
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); got.updates[5301].Profile.Key != "near" || packs != 1 {
		t.Fatalf("winning profile/pack count: %+v packs=%d", got.updates, packs)
	}
	// 低优先级变化以及同来源重试都不应使生效视图重发。
	bind(far, entity.SyncProfile{Key: "farther", LOD: 4})
	bind(near, entity.SyncProfile{Key: "near", LOD: 1})
	mustFlush(t, manager)
	if len(transport.take(1)) != 0 {
		t.Fatal("unchanged active profile resent content")
	}
	// 默认 API 是第三个独立来源，释放它也不能撤销其他来源。
	mustSubscribe(t, manager, 1, 5301, entity.SyncProfile{Key: "default-source", LOD: 9})
	if err := manager.Unsubscribe(1, 5301); err != nil {
		t.Fatal(err)
	}
	if err := near.Unsubscribe(1, 5301); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); !got.updates[5301].Full || got.updates[5301].Profile.Key != "farther" {
		t.Fatal("profile fallback must resend full content")
	}
	if err := far.Unsubscribe(1, 5301); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	got := oneFrame(t, transport, 1)
	if len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatal("final source did not remove object")
	}
	if len(manager.Subscribers(5301)) != 0 {
		t.Fatal("idempotent subscribes leaked ownership")
	}
}

func TestProfilePriorityHasDeterministicTieBreakers(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	if err := manager.Register(testSubject(t, 5302, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	sources := []*SubscriptionSource{manager.NewSubscriptionSource(), manager.NewSubscriptionSource(), manager.NewSubscriptionSource()}
	profiles := []entity.SyncProfile{{Key: "b", LOD: 1, SchemaVersion: 1}, {Key: "a", LOD: 1, SchemaVersion: 2}, {Key: "a", LOD: 1, SchemaVersion: 1}}
	for i, source := range sources {
		if err := source.Subscribe(1, 5302, profiles[i]); err != nil {
			t.Fatal(err)
		}
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); got.updates[5302].Profile != profiles[2] {
		t.Fatalf("winner: %+v", got.updates[5302].Profile)
	}
	if err := sources[2].Unsubscribe(1, 5302); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	if got := oneFrame(t, transport, 1); !got.updates[5302].Full || got.updates[5302].Profile != profiles[1] {
		t.Fatal("schema fallback lost")
	}
}

func TestRetirementCannotBeUndoneByReleasingOneSource(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	if err := manager.Register(testSubject(t, 5303, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	a, b := manager.NewSubscriptionSource(), manager.NewSubscriptionSource()
	if err := a.Subscribe(1, 5303, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(1, 5303, entity.SyncProfile{LOD: 2}); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	transport.take(1)
	if err := manager.Unregister(5303); err != nil {
		t.Fatal(err)
	}
	_ = a.Unsubscribe(1, 5303)
	mustFlush(t, manager)
	got := oneFrame(t, transport, 1)
	if len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove || manager.Stats().Subjects != 0 {
		t.Fatal("source release revived a retiring subject")
	}
}

// HoldSession 开新 epoch 后，接收端要靠新 epoch 的 Full 才知道旧对象作废。held 期间（或 Ready 之后、Full 之前）
// 撤掉最后一个订阅时没有对象可放进 Full，Ready 之后必须补一个空 Full；否则接收端永远留着旧对象。
// 从未收过帧的会话没有旧状态可清，Ready 后不发空帧。
func TestHoldThenReadyWithoutSubscriptionsClearsTheReceiver(t *testing.T) {
	for _, removal := range []string{"unsubscribe_while_held", "unsubscribe_after_ready", "unregister_while_held"} {
		t.Run(removal, func(t *testing.T) {
			transport := newRecordingTransport()
			manager := newTestManager(t, transport, ManagerConfig{})
			packs := 0
			state := testSubject(t, 3002, &packs)
			if err := manager.Register(state); err != nil {
				t.Fatal(err)
			}
			open(t, manager, 1)
			if err := manager.OpenHeldSession(2); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, manager, 1, 3002, entity.SyncProfile{})
			mustFlush(t, manager)
			receiver := frame.NewReceiver(frame.DefaultLimits())
			held := map[frame.ObjectRef]bool{}
			var epoch uint32
			consume := func() int {
				frames := transport.take(1)
				for _, raw := range frames {
					applied, err := receiver.Receive(raw, func(f frame.Frame) error {
						if f.Kind == frame.Full {
							clear(held)
						}
						for _, object := range f.Objects {
							if object.Operation == frame.ObjectRemove {
								delete(held, object.Ref)
							} else {
								held[object.Ref] = true
							}
						}
						epoch = f.Epoch
						return nil
					})
					if err != nil || !applied {
						t.Fatalf("receiver refused a fresh frame: applied=%v err=%v", applied, err)
					}
				}
				return len(frames)
			}
			if consume() != 1 || len(held) != 1 {
				t.Fatalf("the first snapshot did not reach the receiver: %v", held)
			}
			firstEpoch := epoch

			if err := manager.HoldSession(1); err != nil {
				t.Fatal(err)
			}
			switch removal {
			case "unsubscribe_while_held":
				if err := manager.Unsubscribe(1, 3002); err != nil {
					t.Fatal(err)
				}
			case "unregister_while_held":
				if err := manager.Unregister(3002); err != nil {
					t.Fatal(err)
				}
			}
			if err := manager.ReadySession(1); err != nil {
				t.Fatal(err)
			}
			if removal == "unsubscribe_after_ready" {
				if err := manager.Unsubscribe(1, 3002); err != nil {
					t.Fatal(err)
				}
			}
			if err := manager.ReadySession(2); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				mustFlush(t, manager)
			}
			if got := consume(); got != 1 || len(held) != 0 || epoch <= firstEpoch {
				t.Fatalf("after hold, removal and ready the receiver got %d frame(s), still holds %d object(s), epoch %d (was %d); want one empty Full in a newer epoch", got, len(held), epoch, firstEpoch)
			}
			if got := transport.take(2); len(got) != 0 {
				t.Fatalf("a session that never received a frame got %d frame(s) after ready", len(got))
			}
			mustFlush(t, manager)
			if got := transport.take(1); len(got) != 0 {
				t.Fatalf("the empty Full was sent again: %d frame(s)", len(got))
			}
			// 空 Full 之后的新订阅是同一 epoch 里的增量 create。
			if removal != "unregister_while_held" {
				mustSubscribe(t, manager, 1, 3002, entity.SyncProfile{})
				mustFlush(t, manager)
				if consume() != 1 || len(held) != 1 {
					t.Fatalf("a subscription after the empty Full did not arrive: %v", held)
				}
			}
		})
	}
}

// 空 Full 和普通帧一样经传输准入：RetryLater 时这一轮不算交付，下一次 Flush 重发；Drain 等它发出才返回。
func TestEmptyFullAfterHoldIsRetriedUntilAdmitted(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	if err := manager.Register(testSubject(t, 3003, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 3003, entity.SyncProfile{})
	mustFlush(t, manager)
	first := oneFrame(t, transport, 1)
	if err := manager.HoldSession(1); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unsubscribe(1, 3003); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	transport.setRetry(ErrRetryLater)
	if err := manager.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("flush with a refusing transport: %v, want ErrRetryLater", err)
	}
	transport.setRetry(nil)
	if err := manager.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := oneFrame(t, transport, 1)
	if got.wire.Kind != frame.Full || len(got.wire.Objects) != 0 || got.wire.Epoch <= first.wire.Epoch {
		t.Fatalf("after the retry the session got kind=%v objects=%d epoch=%d (was %d), want an empty Full in a newer epoch", got.wire.Kind, len(got.wire.Objects), got.wire.Epoch, first.wire.Epoch)
	}
}

// 后一个会话拒绝后，全局内容版本尚未提交，但前一个客户端已应用增量。
// 重试必须恢复成可应用的全量，不能再次交付以旧版本为基线的增量。
func TestPartialTickRetryRestoresAnApplicableSubjectBaseline(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1201, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2)
	for _, sid := range []SessionID{1, 2} {
		mustSubscribe(t, manager, sid, 1201, entity.SyncProfile{})
	}
	mustFlush(t, manager)
	transport.take(1)
	transport.take(2)

	state.MarkDirty(1)
	transport.breakSession(2, ErrRetryLater)
	if err := manager.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("partial tick: %v", err)
	}
	delivered := oneFrame(t, transport, 1)
	if !state.PendingDirty() {
		t.Fatal("retry consumed the subject's dirty state")
	}
	// 下一次捕获还可能包含失败之后发生的新变化。
	state.MarkDirty(2)
	transport.breakSession(2, nil)
	mustFlush(t, manager)
	for _, sid := range []SessionID{1, 2} {
		retried := oneFrame(t, transport, sid)
		update := retried.updates[1201]
		if sid == 1 {
			if retried.wire.BaseTick != delivered.wire.Tick {
				t.Fatalf("frame base %d, client holds %d", retried.wire.BaseTick, delivered.wire.Tick)
			}
			if !update.Full && update.BaseVersion != delivered.updates[1201].Version {
				t.Fatalf("retry delta starts at %d but client holds %d", update.BaseVersion, delivered.updates[1201].Version)
			}
		}
		if update.Version != state.Version() {
			t.Fatalf("session %d version %d, committed %d", sid, update.Version, state.Version())
		}
	}
	if state.PendingDirty() || manager.Stats().SessionsLost != 0 {
		t.Fatal("successful retry must commit without losing a session")
	}
	if got := manager.Stats().FramesAdmitted; got != 5 {
		t.Fatalf("admitted frames = %d, want initial 2 + partial 1 + retry 2", got)
	}
}

// 一次 Flush 可拆成多帧。第一帧已交付、第二帧拒绝时，重试的帧时钟必须
// 接着客户端最后收到的帧继续，不能倒退到 Flush 开始前。
func TestPartialSessionRetryContinuesAfterTheLastAdmittedFrame(t *testing.T) {
	recorder := newRecordingTransport()
	calls, failAt := 0, 0
	transport := TransportFunc(func(ctx context.Context, sid SessionID, payload []byte) error {
		calls++
		if calls == failAt {
			return ErrRetryLater
		}
		return recorder.Push(ctx, sid, payload)
	})
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 2}})
	open(t, manager, 1)
	packs := 0
	for _, id := range []int64{1, 2, 3, 4} {
		if err := manager.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{1, 2} {
		mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, manager)
	oneFrame(t, recorder, 1)
	for _, id := range []int64{1, 2} {
		if err := manager.Unsubscribe(1, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{3, 4} {
		mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	}
	failAt = calls + 2
	if err := manager.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("partial session tick: %v", err)
	}
	admitted := oneFrame(t, recorder, 1)
	if len(admitted.wire.Objects) != 2 || admitted.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("expected the removal frame before the outage: %+v", admitted.wire)
	}
	failAt = 0
	mustFlush(t, manager)
	frames := recorder.take(1)
	if len(frames) == 0 {
		t.Fatal("retry did not deliver the replacements")
	}
	lastTick := admitted.wire.Tick
	created := 0
	for _, payload := range frames {
		got := decodeFrame(t, payload)
		if got.wire.BaseTick != lastTick {
			t.Fatalf("retry frame base %d, client already holds tick %d", got.wire.BaseTick, lastTick)
		}
		lastTick = got.wire.Tick
		for _, object := range got.wire.Objects {
			if object.Operation != frame.ObjectCreate {
				t.Fatalf("retry repeated an already delivered removal: %+v", object)
			}
			created++
		}
	}
	if created != 2 || manager.Stats().SessionsLost != 0 {
		t.Fatalf("created %d objects, stats %+v", created, manager.Stats())
	}
	if got := manager.Stats().FramesAdmitted; got != 3 {
		t.Fatalf("admitted frames = %d, want initial 1 + partial 1 + retry 1", got)
	}
}
