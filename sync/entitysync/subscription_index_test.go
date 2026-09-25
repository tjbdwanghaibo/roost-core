package entitysync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
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
