package entitysync

import (
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// OPEN-ITEMS B39（第五轮审计疑点，推断）：Unregister 无锁取到 subject、取 subj.mu 之前，旧 subject 被 forget，同 ID 重新
// 登记并由政策重新订阅。承诺（RR-20260926-85）：一次释放只丢掉它所指那次登记上的订阅——新登记上做的订阅（戳更晚）
// 不受影响；业务注销没有退役新登记时，新登记上的订阅与待交还的撤销记录都不应被这次注销清掉。
//
// 政策按 SubscriptionSourceHooks.Released 的文档规则簿记（与 policy.Direct.released 相同：戳小于释放戳的才删除并
// Unsubscribe），窗口由 unregisterLookedUp 测试缝确定性进入。
func TestUnregisterOfAForgottenSubjectDoesNotReleaseTheNextRegistration(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 7)
	const id = 8

	var (
		mu    sync.Mutex
		bound = map[RetractedSubscription]SubscriptionStamp{}
	)
	var source *SubscriptionSource
	bind := func(session SessionID, subject int64) {
		stamp, err := source.SubscribeStamped(session, subject, entity.SyncProfile{})
		if err != nil {
			t.Fatalf("bind %d->%d: %v", session, subject, err)
		}
		mu.Lock()
		bound[RetractedSubscription{Session: session, Subject: subject}] = stamp
		mu.Unlock()
	}
	source = manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{
		Resubmit: func(batch []RetractedSubscription) {
			for _, pair := range batch {
				mu.Lock()
				_, still := bound[pair]
				mu.Unlock()
				if still {
					bind(pair.Session, pair.Subject)
				}
			}
		},
		Released: func(pairs []ReleasedSubscription) {
			mu.Lock()
			defer mu.Unlock()
			for _, pair := range pairs {
				key := RetractedSubscription{Session: pair.Session, Subject: pair.Subject}
				if stamp, ok := bound[key]; !ok || stamp > pair.Stamp {
					continue
				}
				delete(bound, key)
				_ = source.Unsubscribe(pair.Session, pair.Subject)
			}
		},
	})

	first := labelledSubject(id, "first")
	if err := manager.Register(first); err != nil {
		t.Fatal(err)
	}
	bind(7, id)
	mustFlush(t, manager)
	transport.take(7)
	// RR-59：卸载后重载不了，退回 remove；remove 还没发出，旧 subject 仍在表里（退役中）。
	first.Close()
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: retract refused")
	}

	var (
		newStamp   SubscriptionStamp
		windowSeen bool
	)
	manager.unregisterLookedUp = func(int64) {
		manager.unregisterLookedUp = nil
		windowSeen = true
		// 窗口内：旧 subject 的 remove 发出、被 forget；实体重新登记，政策在新登记上重新订阅（交还尚未交付）。
		mustFlush(t, manager)
		if got := manager.subject(id); got != nil {
			t.Fatalf("premise: the old subject was not forgotten in the window: %v", got.id)
		}
		if err := manager.Register(labelledSubject(id, "second")); err != nil {
			t.Fatal(err)
		}
		bind(7, id)
		mu.Lock()
		newStamp = bound[RetractedSubscription{Session: 7, Subject: id}]
		mu.Unlock()
	}
	if err := manager.Unregister(id); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if !windowSeen {
		t.Fatal("premise: the window between lookup and lock was not entered")
	}
	mustFlush(t, manager)
	mustFlush(t, manager)

	registered := manager.subject(id) != nil
	mu.Lock()
	stamp, stillBound := bound[RetractedSubscription{Session: 7, Subject: id}]
	mu.Unlock()
	holds := false
	for _, sid := range manager.Subscribers(id) {
		holds = holds || sid == 7
	}
	t.Logf("after Unregister of the forgotten subject: new registration still registered=%v, policy binding kept=%v (stamp %d, bound at %d), session 7 subscribed=%v",
		registered, stillBound, stamp, newStamp, holds)
	if registered && (!stillBound || !holds) {
		t.Fatalf("Unregister looked up the old subject, which was forgotten before it took the lock; the new registration stayed registered, "+
			"but its subscription made after the re-registration (stamp %d) was released by this Unregister: policy binding kept=%v, session 7 subscribed=%v",
			newStamp, stillBound, holds)
	}
}

// RR-20260927-22：同一窗口的另一半——旧 subject 已被 forget 摘表、forgotten 标记还没打上（forgetUnlinked 窗口）时 Unregister
// 取到了锁。只看 forgotten 标记挡不住这一半，必须确认取到的仍是表里当前那一个。修复选择的语义：按 ID 注销的是当前登记，
// 所以重新登记的那一个被退役，政策在新登记上的订阅（戳更早于本次释放）随之释放，按 ID 待交还的记录也不再留下。
func TestUnregisterBetweenUnlinkAndForgottenRetiresTheCurrentRegistration(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 7)
	const id = 8

	var (
		mu    sync.Mutex
		bound = map[RetractedSubscription]SubscriptionStamp{}
	)
	var source *SubscriptionSource
	bind := func(session SessionID, subject int64) {
		stamp, err := source.SubscribeStamped(session, subject, entity.SyncProfile{})
		if err != nil {
			t.Errorf("bind %d->%d: %v", session, subject, err)
			return
		}
		mu.Lock()
		bound[RetractedSubscription{Session: session, Subject: subject}] = stamp
		mu.Unlock()
	}
	source = manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{
		Resubmit: func(batch []RetractedSubscription) {
			for _, pair := range batch {
				mu.Lock()
				_, still := bound[pair]
				mu.Unlock()
				if still {
					bind(pair.Session, pair.Subject)
				}
			}
		},
		Released: func(pairs []ReleasedSubscription) {
			mu.Lock()
			defer mu.Unlock()
			for _, pair := range pairs {
				key := RetractedSubscription{Session: pair.Session, Subject: pair.Subject}
				if stamp, ok := bound[key]; !ok || stamp > pair.Stamp {
					continue
				}
				delete(bound, key)
				_ = source.Unsubscribe(pair.Session, pair.Subject)
			}
		},
	})

	first := labelledSubject(id, "first")
	if err := manager.Register(first); err != nil {
		t.Fatal(err)
	}
	bind(7, id)
	mustFlush(t, manager)
	transport.take(7)
	first.Close()
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: retract refused")
	}

	// Unregister 取到旧 subject 后停在取锁之前；旧 subject 的 remove 由下面的 Flush 发出并进入 forget。
	lookedUp, proceed, unregistered := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	manager.unregisterLookedUp = func(int64) {
		manager.unregisterLookedUp = nil
		close(lookedUp)
		<-proceed
	}
	go func() { unregistered <- manager.Unregister(id) }()
	<-lookedUp

	var (
		newStamp   SubscriptionStamp
		windowSeen bool
		unregErr   error
	)
	manager.forgetUnlinked = func(int64) {
		manager.forgetUnlinked = nil
		windowSeen = true
		// 旧 subject 已离表、forgotten 尚未标记：同 ID 重新登记，政策在新登记上订阅；然后放 Unregister 取锁并等它返回。
		if err := manager.Register(labelledSubject(id, "second")); err != nil {
			t.Error(err)
		}
		bind(7, id)
		mu.Lock()
		newStamp = bound[RetractedSubscription{Session: 7, Subject: id}]
		mu.Unlock()
		close(proceed)
		unregErr = <-unregistered
	}
	mustFlush(t, manager)
	if !windowSeen {
		t.Fatal("premise: the forget window between unlink and the forgotten mark was not entered")
	}
	if unregErr != nil {
		t.Fatalf("unregister: %v", unregErr)
	}
	mustFlush(t, manager)
	mustFlush(t, manager)

	registered := manager.subject(id) != nil
	mu.Lock()
	stamp, stillBound := bound[RetractedSubscription{Session: 7, Subject: id}]
	mu.Unlock()
	holds := false
	for _, sid := range manager.Subscribers(id) {
		holds = holds || sid == 7
	}
	t.Logf("after Unregister between unlink and forgotten: registered=%v, policy binding kept=%v (stamp %d, bound at %d), session 7 subscribed=%v, pending hand-back records=%d",
		registered, stillBound, stamp, newStamp, holds, manager.resubmitEntries.Load())
	if registered {
		t.Fatalf("Unregister(%d) returned nil but the registration made after the old subject left the table is still registered "+
			"(policy binding kept=%v, session 7 subscribed=%v): it acted on the unlinked old subject instead of the current one", id, stillBound, holds)
	}
	if stillBound || holds {
		t.Fatalf("the current registration was retired but the policy binding made on it (stamp %d) was not released: kept=%v, subscribed=%v", newStamp, stillBound, holds)
	}
	if n := manager.resubmitEntries.Load(); n != 0 {
		t.Fatalf("hand-back records of the retired registration were left behind: %d", n)
	}
}
