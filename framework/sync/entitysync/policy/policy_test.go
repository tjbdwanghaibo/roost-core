// 订阅政策：自身、距离、关系、组、直连各自的订阅与释放规则，被拒的订阅重说，重开会话恢复视图，profile 组装。

package policy

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// ARCH-10 · M-14：组织方式只决定"谁订谁"，全部落到 Manager 的 Subscribe / Unsubscribe 上。
// 这里的承诺原来在 demo 的 InterestSystem 测试里（self 经关系源、滞回不抖、关系比距离活得久、
// 被拒重发），搬进 core 后改为对 Manager 的订阅表断言；Group / Direct 是新增的两种组织方式。

// sink records frames per session; the policies never see it.
type sink struct {
	mu     sync.Mutex
	frames map[entitysync.SessionID]int
}

func (s *sink) Push(_ context.Context, session entitysync.SessionID, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frames == nil {
		s.frames = make(map[entitysync.SessionID]int)
	}
	s.frames[session]++
	return nil
}

func newManager(t *testing.T) *entitysync.Manager {
	t.Helper()
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return manager
}

func subjectState(t *testing.T, id int64) *entity.SubjectSyncState {
	t.Helper()
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, Namespace: "test",
		Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.TakeFrozenSyncPayload(1, []byte("snapshot")), nil
			},
			Delta: func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
				return entity.TakeFrozenSyncPayload(1, []byte("delta")), nil
			},
		},
	})
}

// player registers a subject and opens its session: what the game's join path
// does before the policy sees the id.
func player(t *testing.T, manager *entitysync.Manager, id int64) {
	t.Helper()
	if err := manager.Register(subjectState(t, id)); err != nil {
		t.Fatalf("register %d: %v", id, err)
	}
	if err := manager.OpenSession(entitysync.SessionID(id)); err != nil {
		t.Fatalf("open session %d: %v", id, err)
	}
}

// subscribed asks the manager after a tick: an unsubscribed pair stays listed
// until the remove has gone out, which is the tick's job.
func subscribed(manager *entitysync.Manager, observer, subject int64) bool {
	_ = manager.Flush(context.Background())
	return slices.Contains(manager.Subscribers(subject), entitysync.SessionID(observer))
}

// 这些用例用同一个小整数同时当玩家 ID 与它的实体 ID（玩家 n 的实体也是 n）。玩家 ID 与实体 ID 分开的情形见
// TestObserverAndSubjectIDSpacesStayApart。
func newInterest(t *testing.T, manager *entitysync.Manager) *Interest {
	t.Helper()
	return newInterestMode(t, manager, ApplySync)
}

// newInterestMode 同 newInterest，指定排队事实的应用方式（语义用例对两种模式各跑一遍，见 forEachApplyMode）。
func newInterestMode(t *testing.T, manager *entitysync.Manager, mode ApplyMode) *Interest {
	t.Helper()
	interest, err := NewInterest(InterestConfig{
		Manager: manager, ApplyMode: mode,
		AOI: AOIConfig{
			Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
			EnterRadius: 120, LeaveRadius: 150,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	return interest
}

// setRelation 用一个只挂给 player 的关系视野表达“player 关心这些实体”：视野 ID 取玩家 ID，第一次用到时定义，
// 每次都挂接（已挂接的不变）并替换成员。subjects 为空即清空。
func setRelation(t *testing.T, interest *Interest, player int64, subjects ...int64) {
	t.Helper()
	id := ViewID(player)
	if err := interest.DefineRelationView(id, entity.SyncProfile{}); err != nil && !errors.Is(err, ErrViewExists) {
		t.Fatal(err)
	}
	if err := interest.AttachView(player, id); err != nil {
		t.Fatal(err)
	}
	if err := interest.SetViewMembers(id, subjects); err != nil {
		t.Fatal(err)
	}
}

func mustApply(t *testing.T, interest *Interest) {
	t.Helper()
	if refusals := interest.Apply(); len(refusals) != 0 {
		t.Fatalf("apply refused: %+v", refusals)
	}
}

const (
	watcherID = int64(1001)
	moverID   = int64(1002)
)

// 进场就订阅自己——经 self，不经距离：空间视野不看自己跟随的实体。
func TestEnteringSubscribesToYourself(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if !subscribed(manager, watcherID, watcherID) {
		t.Fatalf("entering did not subscribe the player to itself: %v", manager.Subscribers(watcherID))
	}
}

// 距离订阅与释放；滞回：在两个半径之间抖动不产生任何订阅变化。
func TestDistanceSubscribesReleasesAndDoesNotChurnOnTheBoundary(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	player(t, manager, moverID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, moverID, spatial.Point{X: 500 + 119, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if !subscribed(manager, watcherID, moverID) {
		t.Fatal("a player 119 units away was not subscribed")
	}
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := manager.Stats().FramesAdmitted
	for _, x := range []int64{125, 140, 121, 149, 130} {
		if err := interest.Move(moverID, spatial.Point{X: 500 + x, Y: 500}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, interest)
		if err := manager.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !subscribed(manager, watcherID, moverID) || manager.Stats().FramesAdmitted != before {
		t.Fatalf("jittering between the radii churned the subscription: subscribed=%v frames=%d→%d", subscribed(manager, watcherID, moverID), before, manager.Stats().FramesAdmitted)
	}
	if err := interest.Move(moverID, spatial.Point{X: 700, Y: 500}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("a player 200 units away was not released")
	}
}

// 关系视野比距离活得久：队友走出视野不撤订阅，退队之后（最后一个视野）才撤。
func TestARelationKeepsASubscriptionDistanceDropped(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	player(t, manager, moverID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, moverID, spatial.Point{X: 520, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	setRelation(t, interest, watcherID, moverID)
	mustApply(t, interest)
	if !subscribed(manager, watcherID, moverID) {
		t.Fatal("the teammate was not subscribed")
	}
	if err := interest.Move(moverID, spatial.Point{X: 900, Y: 500}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if !subscribed(manager, watcherID, moverID) {
		t.Fatal("walking out of view released a teammate")
	}
	setRelation(t, interest, watcherID)
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("the last source went away and the subscription survived")
	}
}

// 离场双向释放，调用方不用记得谁在看。
func TestLeavingReleasesBothDirections(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	player(t, manager, moverID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, moverID, spatial.Point{X: 520, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	setRelation(t, interest, watcherID, moverID)
	mustApply(t, interest)
	if err := interest.Leave(moverID, moverID); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) || subscribed(manager, moverID, moverID) || subscribed(manager, moverID, watcherID) {
		t.Fatalf("a leaver is still held somewhere: %v / %v", manager.Subscribers(moverID), manager.Subscribers(watcherID))
	}
}

// 被拒的 subscribe 在每次 Apply 再说一次，直到被接受或 pair 被释放：队伍在队友入场之前就成了，第一次订阅因 subject 未注册被拒，
// 队友入场后的 Apply 补上。
func TestARefusedSubscribeIsSaidAgainUntilItIsTaken(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 100, Y: 100}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	// The teammate has not joined: no subject, no session.
	setRelation(t, interest, watcherID, moverID)
	refusals := interest.Apply()
	if len(refusals) != 1 || refusals[0].Retry || !errors.Is(refusals[0].Err, entitysync.ErrSubjectNotRegistered) {
		t.Fatalf("first refusal = %+v, want one non-retry refusal for the unregistered teammate", refusals)
	}
	// Still refused, now marked as a retry so the caller can log it quietly.
	refusals = interest.Apply()
	if len(refusals) != 1 || !refusals[0].Retry {
		t.Fatalf("second refusal = %+v, want one retry", refusals)
	}
	player(t, manager, moverID)
	if err := interest.Enter(moverID, moverID, spatial.Point{X: 900, Y: 900}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if !subscribed(manager, watcherID, moverID) {
		t.Fatal("the retried subscribe was not taken once the teammate joined")
	}
	// Taken: the retry list is empty, an idle Apply says nothing.
	if refusals := interest.Apply(); len(refusals) != 0 {
		t.Fatalf("an idle apply refused: %+v", refusals)
	}
}

// 在拒绝到达之前已经释放的 pair 不重试、也不撤订阅——Manager 从未接受过它。
func TestARefusedSubscribeForAReleasedPairIsDropped(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 100, Y: 100}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	setRelation(t, interest, watcherID, moverID)
	if refusals := interest.Apply(); len(refusals) != 1 {
		t.Fatalf("refusals = %+v", refusals)
	}
	setRelation(t, interest, watcherID)
	if refusals := interest.Apply(); len(refusals) != 0 {
		t.Fatalf("a released pair was still being offered or taken back: %+v", refusals)
	}
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("a pair that was released came back")
	}
}

// Group：成员全互见；subject 加入后所有成员都订上；成员离开撤掉；关房退役全部 subject。
func TestAGroupIsAllToAll(t *testing.T) {
	manager := newManager(t)
	group, err := NewGroup(GroupConfig{Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2, 3} {
		if err := manager.OpenSession(entitysync.SessionID(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := group.AddSubject(subjectState(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := group.Join(1); err != nil {
		t.Fatal(err)
	}
	if err := group.Join(2); err != nil {
		t.Fatal(err)
	}
	// A subject added after the members joined reaches them too.
	if err := group.AddSubject(subjectState(t, 2)); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []int64{1, 2} {
		for _, member := range []int64{1, 2} {
			if !subscribed(manager, member, subject) {
				t.Fatalf("member %d does not receive subject %d: %v", member, subject, manager.Subscribers(subject))
			}
		}
	}
	if subscribed(manager, 3, 1) {
		t.Fatal("a session that never joined receives the group")
	}
	if err := group.Leave(2); err != nil {
		t.Fatal(err)
	}
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if subscribed(manager, 2, 1) || subscribed(manager, 2, 2) {
		t.Fatal("a member who left still receives the group")
	}
	if err := group.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.Stats().Subjects != 2 || manager.Stats().Subscriptions != 0 {
		t.Fatalf("closing a group must release its subscriptions, not retire entities: %+v", manager.Stats())
	}
	for _, id := range []int64{1, 2} {
		if err := manager.Unregister(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := group.Join(3); !errors.Is(err, ErrGroupClosed) {
		t.Fatalf("join after close: %v", err)
	}
}

// Direct：一个绑定就是一个订阅。
func TestDirectBindsOnePairAtATime(t *testing.T) {
	manager := newManager(t)
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	player(t, manager, 7)
	player(t, manager, 8)
	if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if !subscribed(manager, 7, 8) || subscribed(manager, 8, 7) {
		t.Fatalf("bind is not one-directional: %v / %v", manager.Subscribers(8), manager.Subscribers(7))
	}
	if err := direct.Unbind(7, 8); err != nil {
		t.Fatal(err)
	}
	if subscribed(manager, 7, 8) {
		t.Fatal("unbind left the subscription")
	}
}

// Manager 因传输失败自行丢掉一个会话而观察者仍在政策里时，重开会话后 Resubscribe 把该观察者持有的全部 pair 重说一次；其他观察者
// 对它的订阅不受影响。
// 历史：RR-20260926-40。
func TestResubscribeRestoresAReopenedObserversView(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	player(t, manager, moverID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, moverID, spatial.Point{X: 520, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	setRelation(t, interest, watcherID, moverID)
	mustApply(t, interest)
	if !subscribed(manager, watcherID, moverID) || !subscribed(manager, moverID, watcherID) {
		t.Fatal("setup: the two players do not see each other")
	}

	// The manager drops the watcher's session the way a failed push does, and
	// the session is opened again for the connection that is still up.
	manager.CloseSession(entitysync.SessionID(watcherID))
	if err := manager.OpenSession(entitysync.SessionID(watcherID)); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("setup: the reopened session kept a subscription the manager dropped")
	}
	if got := interest.Resubscribe(watcherID); got != 2 {
		t.Fatalf("Resubscribe queued %d pairs, want 2 (self and the mover)", got)
	}
	mustApply(t, interest)
	if !subscribed(manager, watcherID, moverID) || !subscribed(manager, watcherID, watcherID) {
		t.Fatalf("the reopened session's view was not restored: %v / %v", manager.Subscribers(moverID), manager.Subscribers(watcherID))
	}
	if !subscribed(manager, moverID, watcherID) {
		t.Fatal("resubscribing the watcher disturbed the mover's own subscription to it")
	}
	// Nothing left over: an idle Apply says nothing, and the pair still
	// releases normally on leave.
	if refusals := interest.Apply(); len(refusals) != 0 {
		t.Fatalf("an idle apply refused: %+v", refusals)
	}
	if err := interest.Leave(moverID, moverID); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("a resubscribed pair did not release on leave")
	}
}

func TestGroupAddFailureCanBeRetriedWithoutPartialMembership(t *testing.T) {
	manager := newManager(t)
	group, err := NewGroup(GroupConfig{Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := group.Join(id); err != nil {
			t.Fatal(err)
		}
	}
	state := subjectState(t, 5101)
	if err := group.AddSubject(state); err == nil {
		t.Fatal("missing session should refuse AddSubject")
	}
	if group.Subjects() != 0 || len(manager.Subscribers(5101)) != 0 {
		t.Fatal("failed addition left partial group membership")
	}
	if err := manager.OpenSession(2); err != nil {
		t.Fatal(err)
	}
	if err := group.AddSubject(state); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(manager.Subscribers(5101)) != 2 {
		t.Fatal("retry did not subscribe both members")
	}
}

func TestPoliciesReleaseOnlyTheirOwnSubscriptions(t *testing.T) {
	for _, operation := range []string{"leave", "remove", "close", "direct"} {
		t.Run(operation, func(t *testing.T) {
			manager := newManager(t)
			player(t, manager, 1)
			player(t, manager, 2)
			group, err := NewGroup(GroupConfig{Manager: manager})
			if err != nil {
				t.Fatal(err)
			}
			if err := group.AddSubject(subjectState(t, 2)); err != nil {
				t.Fatal(err)
			}
			if err := group.Join(1); err != nil {
				t.Fatal(err)
			}
			direct, err := NewDirect(manager, nil)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := direct.Bind(1, 2, entity.SyncProfile{}); err != nil {
					t.Fatal(err)
				}
			}
			switch operation {
			case "leave":
				err = group.Leave(1)
			case "remove":
				err = group.RemoveSubject(2)
			case "close":
				err = group.Close()
			case "direct":
				err = direct.Unbind(1, 2)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !subscribed(manager, 1, 2) {
				t.Fatal("one policy revoked another policy's subscription")
			}
			if operation == "direct" {
				err = group.Leave(1)
			} else {
				err = direct.Unbind(1, 2)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if subscribed(manager, 1, 2) {
				t.Fatal("repeated Bind leaked a subscription")
			}
		})
	}
}

func TestGroupRejoinRestoresReopenedSession(t *testing.T) {
	m := newManager(t)
	player(t, m, 1)
	player(t, m, 2)
	g, err := NewGroup(GroupConfig{Manager: m, MaxMembers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.AddSubject(subjectState(t, 2)); err != nil {
		t.Fatal(err)
	}
	if err = g.Join(1); err != nil {
		t.Fatal(err)
	}
	m.CloseSession(entitysync.SessionID(1))
	if err = m.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	_ = g.Join(1)
	if !subscribed(m, 1, 2) {
		t.Fatal("rejoining a reopened member did not restore its view")
	}
	if g.Members() != 1 {
		t.Fatal("rejoin changed membership count")
	}
	if err = g.Leave(1); err != nil {
		t.Fatal(err)
	}
	if subscribed(m, 1, 2) {
		t.Fatal("leave retained restored view")
	}
}

func directBindings(d *Direct) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.bound)
}

func TestDirectForgetsBindingsOfClosedSessionsAndUnregisteredSubjects(t *testing.T) {
	manager, _ := newLoggedManager(t)
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 1000
	for i := int64(0); i < n; i++ {
		observer, subject := 10000+i, 20000+i
		player(t, manager, observer)
		if err := manager.Register(subjectState(t, subject)); err != nil {
			t.Fatal(err)
		}
		if err := direct.Bind(observer, subject, entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	flushTimes(manager, 1)
	for i := int64(0); i < n; i++ {
		observer, subject := 10000+i, 20000+i
		if i%2 == 0 {
			manager.CloseSession(entitysync.SessionID(observer))
		} else if err := manager.Unregister(subject); err != nil {
			t.Fatal(err)
		}
	}
	flushTimes(manager, 3)
	live := 0
	for i := int64(0); i < n; i++ {
		if holds(manager, 10000+i, 20000+i) {
			live++
		}
	}
	if bound := directBindings(direct); bound != live {
		t.Fatalf("Direct keeps %d bindings with no manager subscription behind them (bound=%d live=%d)", bound-live, bound, live)
	}
}

// 通知在下一次政策阶段才到：之间在重开的会话、重新登记的实体上再次 Bind 的绑定是新的，不能被旧通知删掉。
func TestDirectKeepsABindingMadeAgainBeforeTheReleaseArrives(t *testing.T) {
	manager, _ := newLoggedManager(t)
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	player(t, manager, 7)
	if err := manager.Register(subjectState(t, 8)); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 1)
	manager.CloseSession(7)
	if err := manager.OpenSession(7); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	// 实体 9 从未交付给 7（注销后立刻被忘掉），随即以同 ID 重新登记并再次 Bind。
	if err := manager.Register(subjectState(t, 9)); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 9, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unregister(9); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(subjectState(t, 9)); err != nil {
		t.Fatalf("premise: subject 9 was not forgotten at once: %v", err)
	}
	if err := direct.Bind(7, 9, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	if !holds(manager, 7, 8) || !holds(manager, 7, 9) {
		t.Fatalf("premise: rebinding did not subscribe: 8=%v 9=%v", manager.Subscribers(8), manager.Subscribers(9))
	}
	if bound := directBindings(direct); bound != 2 {
		t.Fatalf("bindings made again before the release arrived were dropped: bound=%d, want 2", bound)
	}
}

// 会话在实体缺席（撤销后）期间关闭：绑定随会话删除，实体重新登记后不重新提交到同 ID 重开的新连接上；会话一直开着时撤销不删除
// 绑定（另见 TestGroupAndDirectResubmitAfterRetraction）。
func TestDirectDropsTheBindingOfARetractedSubjectWhenItsSessionCloses(t *testing.T) {
	manager, _ := newLoggedManager(t)
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	player(t, manager, 7)
	state8 := subjectState(t, 8)
	if err := manager.Register(state8); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 1)
	retractUnloaded(t, manager, state8)
	if bound := directBindings(direct); bound != 1 {
		t.Fatalf("the RR-59 retraction dropped the binding (bound=%d); RR-70 needs it to resubmit", bound)
	}
	manager.CloseSession(7)
	flushTimes(manager, 1)
	if bound := directBindings(direct); bound != 0 {
		t.Fatalf("the binding of a closed session outlived it while its subject was away: bound=%d", bound)
	}
	if err := manager.OpenSession(7); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(subjectState(t, 8)); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	if holds(manager, 7, 8) {
		t.Fatalf("a binding of the closed connection was resubmitted to the reopened one: %v", manager.Subscribers(8))
	}
}

func rebindThenRetract(t *testing.T, deliverFirstBinding bool, drop func(*testing.T, *entitysync.Manager, *Direct, *entity.SubjectSyncState) *entity.SubjectSyncState) (int, bool) {
	t.Helper()
	manager, _ := newLoggedManager(t)
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	player(t, manager, 7)
	state8 := subjectState(t, 8)
	if err := manager.Register(state8); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if deliverFirstBinding {
		flushTimes(manager, 1)
	}
	if drop != nil {
		state8 = drop(t, manager, direct, state8)
	}
	retractUnloaded(t, manager, state8)
	bound := directBindings(direct)
	if err := manager.Register(subjectState(t, 8)); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	return bound, holds(manager, 7, 8)
}

func TestAStaleReleaseKeepsARebindingThatIsThenRetracted(t *testing.T) {
	// 对照：没有释放通知时，RR-59 撤销后 Direct 按绑定恢复订阅（RR-70）。
	if bound, restored := rebindThenRetract(t, true, nil); bound != 1 || !restored {
		t.Fatalf("premise: the retraction alone is not restored: bound=%d holds(7,8)=%v", bound, restored)
	}

	t.Run("session closed and reopened", func(t *testing.T) {
		bound, restored := rebindThenRetract(t, true, func(t *testing.T, manager *entitysync.Manager, direct *Direct, state8 *entity.SubjectSyncState) *entity.SubjectSyncState {
			manager.CloseSession(7) // 释放通知指向第一次打开
			if err := manager.OpenSession(7); err != nil {
				t.Fatal(err)
			}
			if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			return state8
		})
		if bound != 1 || !restored {
			t.Fatalf("the release of the closed lifetime dropped the binding made on the reopened session: bound after retraction=%d, after reload holds(7,8)=%v, want 1 and true", bound, restored)
		}
	})

	t.Run("subject unregistered and registered again", func(t *testing.T) {
		// 第一次绑定未交付：Unregister 时 subject 立刻被忘掉，通知仍排着，同 ID 可以马上重新登记。
		bound, restored := rebindThenRetract(t, false, func(t *testing.T, manager *entitysync.Manager, direct *Direct, _ *entity.SubjectSyncState) *entity.SubjectSyncState {
			if err := manager.Unregister(8); err != nil { // 释放通知指向第一次登记
				t.Fatal(err)
			}
			next := subjectState(t, 8)
			if err := manager.Register(next); err != nil {
				t.Fatalf("premise: subject 8 was not forgotten at once: %v", err)
			}
			if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			return next
		})
		if bound != 1 || !restored {
			t.Fatalf("the release of the unregistered subject dropped the binding made on its new registration: bound after retraction=%d, after reload holds(7,8)=%v, want 1 and true", bound, restored)
		}
	})
}

func TestProfileAssemblyChecksFallbackSchemaAndPriority(t *testing.T) {
	near := entity.SyncProfile{Key: "near"}
	views, _ := entity.NewSyncViewSet(entity.SyncView{Profile: near, Fields: 1, Priority: -5})
	priorities, err := entity.MergeSyncViewPriorities(views)
	if err != nil {
		t.Fatal(err)
	}
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, ProfilePriorities: priorities})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	layers := map[int]entity.SyncProfile{0: near}
	bandLayer := func(band int) entity.SyncProfile {
		if layer, ok := layers[band]; ok {
			return layer
		}
		return DefaultBandProfile(band)
	}
	cfg := InterestConfig{Manager: m, AOI: interestConfig(), ViewSets: map[string]*entity.SyncViewSet{"player": views}, SelfLayer: near, BandLayer: bandLayer}
	if _, err := NewInterest(cfg); !errors.Is(err, entity.ErrSyncViewUnknown) {
		t.Fatalf("missing fallback: %v", err)
	}
	layers[1] = near
	layers[2] = near
	in, err := NewInterest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in.Close()
	layers[2] = entity.SyncProfile{Key: "near", SchemaVersion: 2}
	if _, err := NewInterest(cfg); !errors.Is(err, entity.ErrSyncViewUnknown) {
		t.Fatalf("schema mismatch: %v", err)
	}
	layers[2] = near
	conflict, _ := entity.NewSyncViewSet(entity.SyncView{Profile: near, Priority: 2})
	if _, err := entity.MergeSyncViewPriorities(views, conflict); err == nil {
		t.Fatal("conflicting priorities accepted")
	}
	cfg.ViewSets["monster"] = conflict
	if _, err := NewInterest(cfg); err == nil {
		t.Fatal("framework/manager/view priority mismatch accepted")
	}
}

func TestProfileAssemblyCopiesSetsAndRejectsDynamicUnknownView(t *testing.T) {
	known := entity.SyncProfile{Key: "public"}
	views, _ := entity.NewSyncViewSet(entity.SyncView{Profile: known, Fields: 1})
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	sets := map[string]*entity.SyncViewSet{"player": views}
	dynamic := known
	in, err := NewInterest(InterestConfig{Manager: m, AOI: interestConfig(), ViewSets: sets, SelfLayer: known, BandLayer: func(int) entity.SyncProfile { return dynamic }})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	delete(sets, "player")
	player(t, m, 1)
	dynamic = entity.SyncProfile{Key: "not-declared"}
	var refusals []Refusal
	in.contribute(pair{observer: 1, subject: 1}, contribution{view: 1}, in.layerForBand(0), true)
	in.settlePair(pair{observer: 1, subject: 1}, &refusals)
	if len(refusals) != 1 || !errors.Is(refusals[0].Err, entity.ErrSyncViewUnknown) {
		t.Fatalf("refusals: %+v", refusals)
	}
	if len(m.Subscribers(1)) != 0 {
		t.Fatal("invalid profile reached manager")
	}
}

// ---- 观察者编号与实体编号分开时的订阅结果（D14 改签名前后的回归基线） ----
//
// 玩家 ID（会话）与实体 ID 是两种编号：订阅表里会话用玩家 ID，被订阅的目标用实体 ID。用例只对 Manager 的订阅表断言，
// 不读 Interest 内部的键。D14 之前它以“观察者按实体 ID 编号、经 Session 换算”的写法在 D14 第一笔提交（改签名前的回归）上通过；D14 之后观察者直接是
// 玩家 ID、事务内入口经缺省 ObserverOf 换算（D19 起主视野跟随实体、ObserverOf 删除），断言一字未改：自己只经 self 关系看见自己、距离双向、数值等于玩家 ID 的实体不被
// 误当成自己、关系比距离活得久、退场双向清理、排队移动与排队关系按玩家生效。

// playerEntity 拼一个能被 entity.GetUniqueIDFromEntityID 还原成 playerID 的实体 ID（低位填一个非零标记），
// 让实体 ID 与玩家 ID 数值不同。
func playerEntity(playerID int64) int64 {
	return int64(uint64(playerID)<<entity.UniqueIDShift | 4)
}

// registerPlayer 注册玩家实体的同步状态，并按玩家 ID 打开会话。
func registerPlayer(t *testing.T, manager *entitysync.Manager, playerID int64) int64 {
	t.Helper()
	entityID := playerEntity(playerID)
	if err := manager.Register(subjectState(t, entityID)); err != nil {
		t.Fatalf("register %d: %v", entityID, err)
	}
	if err := manager.OpenSession(entitysync.SessionID(playerID)); err != nil {
		t.Fatalf("open session %d: %v", playerID, err)
	}
	return entityID
}

func sessionsOf(manager *entitysync.Manager, subject int64) []entitysync.SessionID {
	_ = manager.Flush(context.Background())
	got := slices.Clone(manager.Subscribers(subject))
	slices.Sort(got)
	return got
}

func requireSessions(t *testing.T, manager *entitysync.Manager, subject int64, want ...entitysync.SessionID) {
	t.Helper()
	if want == nil {
		want = []entitysync.SessionID{}
	}
	got := sessionsOf(manager, subject)
	if got == nil {
		got = []entitysync.SessionID{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("subscribers of %d = %v, want %v", subject, got, want)
	}
}

func TestObserverAndSubjectIDSpacesStayApart(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		manager := newManager(t)
		interest, err := NewInterest(InterestConfig{
			Manager: manager, ApplyMode: mode,
			AOI: AOIConfig{
				Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
				EnterRadius: 120, LeaveRadius: 150,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(interest.Close)
		const playerA, playerB = int64(7), int64(8)
		entityA, entityB := registerPlayer(t, manager, playerA), registerPlayer(t, manager, playerB)
		// 数值上等于玩家 A 的 ID 的目标：观察者与目标编号混用时它会被误当成 A 自己。
		monster := playerA
		if err := manager.Register(subjectState(t, monster)); err != nil {
			t.Fatal(err)
		}
		if err := interest.Enter(playerA, entityA, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		if err := interest.Enter(playerB, entityB, spatial.Point{X: 540, Y: 500}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		if err := interest.Show(monster, spatial.Point{X: 520, Y: 500}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, interest)
		requireSessions(t, manager, entityA, 7, 8)
		requireSessions(t, manager, entityB, 7, 8)
		requireSessions(t, manager, monster, 7, 8)
		if got := interest.Visible(playerA); !slices.Equal(got, []int64{monster, entityB}) {
			t.Fatalf("A sees %v through distance, want the monster and B (never itself)", got)
		}

		// 关系：A 把 B 当队友；B 走远后 A 仍看得见 B，B 看不见 A 与怪。
		setRelation(t, interest, playerA, entityB)
		mustApply(t, interest)
		if err := interest.Move(entityB, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, interest)
		requireSessions(t, manager, entityB, 7, 8)
		requireSessions(t, manager, entityA, 7)
		requireSessions(t, manager, monster, 7)

		// 排队事实：B 的事务把 B 移回来（跟随 B 的主视野一起移动）；提交之后 A 把队友关系清空（视野操作直接调用）。
		eA, eB := factEntity(t, entityA), factEntity(t, entityB)
		mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{eA, eB}, manager)
		if err := interest.QueueMove(eB, spatial.Point{X: 540, Y: 500}); err != nil {
			t.Fatal(err)
		}
		mutation.Admit()
		mutation.Confirm()
		mutation.Release()
		setRelation(t, interest, playerA)
		flushInterest(t, interest, manager)
		requireSessions(t, manager, entityA, 7, 8)
		requireSessions(t, manager, monster, 7, 8)

		// 退场：B 双向退出，其他观察者对 B 的订阅、B 对别人的订阅、B 的 self 都释放。
		if err := interest.Leave(playerB, entityB); err != nil {
			t.Fatal(err)
		}
		mustApply(t, interest)
		requireSessions(t, manager, entityB)
		requireSessions(t, manager, entityA, 7)
		requireSessions(t, manager, monster, 7)
	})
}

// ---- D14：观察者是玩家、视野按观察者 ----

// viewInterest：缺省视野 120/150，视野大小 [60, 270] 按 30 取整，一层 LOD。
func viewInterest(t *testing.T, manager *entitysync.Manager) *Interest {
	t.Helper()
	interest, err := NewInterest(InterestConfig{
		Manager: manager,
		AOI: AOIConfig{
			Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
			EnterRadius: 120, LeaveRadius: 150,
			MinViewRadius: 60, MaxLeaveRadius: 300, ViewRadiusStep: 30,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	return interest
}

// 客户端改视野直接生效：变大时新进入的目标订阅，变小时超出的退订；夹取与量化后的值回给调用方。
func TestSetObserverViewSubscribesWhatCameIntoViewAndReleasesWhatLeft(t *testing.T) {
	manager := newManager(t)
	interest := viewInterest(t, manager)
	player(t, manager, watcherID)
	player(t, manager, moverID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, moverID, spatial.Point{X: 700, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("setup: 200 units away is visible with the default view")
	}
	view, err := interest.SetObserverView(watcherID, ObserverView{Radius: 10_000})
	if err != nil || view != (ObserverView{Radius: 270}) {
		t.Fatalf("SetObserverView = %+v, %v; want the radius clamped to 270", view, err)
	}
	mustApply(t, interest)
	if !subscribed(manager, watcherID, moverID) {
		t.Fatal("a widened view did not subscribe the player that came into range")
	}
	if subscribed(manager, moverID, watcherID) {
		t.Fatal("widening one player's view widened the other's")
	}
	if got, _ := interest.ObserverView(watcherID); got != view {
		t.Fatalf("ObserverView = %+v, want %+v", got, view)
	}
	interest.now = func() time.Time { return time.Now().Add(2 * time.Second) }
	if _, err := interest.SetObserverView(watcherID, ObserverView{Radius: 100}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("a narrowed view kept a player that fell out of range")
	}
	if !subscribed(manager, watcherID, watcherID) {
		t.Fatal("changing the view dropped the player's own subscription")
	}
}

// 限速：同一玩家距上次生效不足 ViewChangeInterval 的请求拒绝并计数；量化后与当前相同的请求什么也不做、不占限速；
// 层级未配置的请求拒绝并计数；不认识的玩家拒绝。
func TestSetObserverViewIsRateLimitedAndIgnoresNoops(t *testing.T) {
	manager := newManager(t)
	interest := viewInterest(t, manager)
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1000, 0)
	interest.now = func() time.Time { return clock }
	rate := func() int64 { return counterValueWith(viewRejectedMetric, "reason", "rate") }
	lod := func() int64 { return counterValueWith(viewRejectedMetric, "reason", "lod") }
	rateBefore, lodBefore := rate(), lod()

	// 125 量化为 120，与当前相同：不重评、不记时间。
	if view, err := interest.SetObserverView(watcherID, ObserverView{Radius: 125}); err != nil || view.Radius != 120 {
		t.Fatalf("no-op request: %+v %v", view, err)
	}
	if _, err := interest.SetObserverView(watcherID, ObserverView{Radius: 200}); err != nil {
		t.Fatalf("first change refused: %v", err)
	}
	clock = clock.Add(500 * time.Millisecond)
	if view, err := interest.SetObserverView(watcherID, ObserverView{Radius: 240}); !errors.Is(err, ErrObserverViewRate) || view.Radius != 180 {
		t.Fatalf("a second change within the interval: %+v %v (200 quantizes to 180)", view, err)
	}
	if rate() != rateBefore+1 {
		t.Fatalf("rate refusals counted %d, want 1", rate()-rateBefore)
	}
	// 与当前相同的请求不受限速影响。
	if _, err := interest.SetObserverView(watcherID, ObserverView{Radius: 190}); err != nil {
		t.Fatalf("a request equal to the current view was refused: %v", err)
	}
	clock = clock.Add(500 * time.Millisecond)
	if view, err := interest.SetObserverView(watcherID, ObserverView{Radius: 240}); err != nil || view.Radius != 240 {
		t.Fatalf("a change after the interval: %+v %v", view, err)
	}
	if _, err := interest.SetObserverView(watcherID, ObserverView{LOD: 1}); !errors.Is(err, ErrObserverView) || lod() != lodBefore+1 {
		t.Fatalf("an unconfigured LOD level: %v (counted %d)", err, lod()-lodBefore)
	}
	if _, err := interest.SetObserverView(moverID, ObserverView{}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("an unknown player: %v", err)
	}
	if _, err := NewInterest(InterestConfig{Manager: manager, AOI: interestConfig(), ViewChangeInterval: -time.Second}); !errors.Is(err, ErrInterestConfig) {
		t.Fatalf("a negative interval was accepted: %v", err)
	}
}

func counterValueWith(name, label, value string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name && metric.Labels[label] == value {
			total += metric.Value
		}
	}
	return total
}

// 只看不被看：Watch 登记的观察者看得见别人，别人看不见它，也没有 self 订阅；MoveView 移动视点，Unwatch 全部释放。
func TestWatchSeesWithoutBeingSeen(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	const spectator = int64(9)
	if err := manager.OpenSession(entitysync.SessionID(spectator)); err != nil {
		t.Fatal(err)
	}
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Watch(spectator, spatial.Point{X: 900, Y: 900}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Watch(watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); !errors.Is(err, ErrInterestConfig) {
		t.Fatalf("a player already in turned into a spectator: %v", err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, watcherID, entitysync.SessionID(watcherID))
	if err := interest.MoveView(spectator, spatial.Point{X: 520, Y: 500}); err != nil {
		t.Fatal(err)
	}
	setRelation(t, interest, spectator, watcherID)
	// 实体 ID 数值恰好等于观战者玩家 ID 的单位：观战者没有自身实体，它照常看得见。
	if err := manager.Register(subjectState(t, spectator)); err != nil {
		t.Fatal(err)
	}
	if err := interest.Show(spectator, spatial.Point{X: 540, Y: 500}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, watcherID, entitysync.SessionID(spectator), entitysync.SessionID(watcherID))
	requireSessions(t, manager, spectator, entitysync.SessionID(spectator), entitysync.SessionID(watcherID))
	if got := interest.Visible(spectator); !slices.Equal(got, []int64{spectator, watcherID}) {
		t.Fatalf("spectator sees %v", got)
	}
	if got := interest.Visible(watcherID); !slices.Equal(got, []int64{spectator}) {
		t.Fatalf("the player sees %v; want only the unit, never the spectator", got)
	}
	if err := interest.Unwatch(spectator); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, watcherID, entitysync.SessionID(watcherID))
	requireSessions(t, manager, spectator, entitysync.SessionID(watcherID))
	if err := interest.MoveView(spectator, spatial.Point{X: 500, Y: 500}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("a spectator that left still moves: %v", err)
	}
}

// 排队移动带着跟随实体的视野走：玩家以英雄（不是 Player 实体）进场，主视野跟随英雄；英雄的事务移动把视野一起带走，
// 别的实体的事务移动只移动它自己。
func TestQueuedMoveCarriesTheViewsThatFollowTheEntity(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		manager := newManager(t)
		const playerID, hero, other = int64(5), int64(500), int64(600)
		interest := newInterestMode(t, manager, mode)
		if err := manager.OpenSession(entitysync.SessionID(playerID)); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{hero, other} {
			if err := manager.Register(subjectState(t, id)); err != nil {
				t.Fatal(err)
			}
		}
		if err := interest.Enter(playerID, hero, spatial.Point{X: 100, Y: 100}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		if err := interest.Show(other, spatial.Point{X: 800, Y: 800}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, interest)
		requireSessions(t, manager, other)
		eHero, eOther := factEntity(t, hero), factEntity(t, other)
		mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{eHero, eOther}, manager)
		if err := interest.QueueMove(eHero, spatial.Point{X: 780, Y: 800}); err != nil {
			t.Fatal(err)
		}
		if err := interest.QueueMove(eOther, spatial.Point{X: 790, Y: 800}); err != nil {
			t.Fatal(err)
		}
		mutation.Admit()
		mutation.Confirm()
		mutation.Release()
		flushInterest(t, interest, manager)
		requireSessions(t, manager, other, entitysync.SessionID(playerID))
		if got := interest.Visible(playerID); !slices.Equal(got, []int64{other}) {
			t.Fatalf("the player's view did not follow the hero: %v", got)
		}
	})
}

// 多对一隐患消失：两个玩家各是一个会话，都看见同一目标；其中一个走远只退它自己的订阅。
// D14 之前同样的场景经 Session 映射到同一会话时，订阅被一起退掉（D14 第一笔提交里的 TestSessionManyToOneDropsASharedSubscription，随 Session 删除）。
func TestTwoObserversDoNotReleaseEachOthersSubscription(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, 11)
	player(t, manager, 12)
	if err := manager.Register(subjectState(t, 20)); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(11, 11, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(12, 12, spatial.Point{X: 500, Y: 520}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Show(20, spatial.Point{X: 520, Y: 500}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, 20, 11, 12)
	if err := interest.Move(12, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, 20, 11)
}

// ---- 关系与多来源合并的回归基线（D19 改结构前在旧 API 上写下，断言只看 Manager 的订阅表与 packer 收到的视图） ----

// profileRecorder 是记录最近一次全量视图的 subject：只有一个会话订阅它时，记录的就是这个会话拿到的视图。
type profileRecorder struct {
	mu   sync.Mutex
	last entity.SyncProfile
}

func (r *profileRecorder) profile() entity.SyncProfile {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func recordingSubject(t *testing.T, manager *entitysync.Manager, id int64) *profileRecorder {
	t.Helper()
	recorder := &profileRecorder{}
	state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "test", Packer: entity.SubjectSyncPackFunc{
		Snapshot: func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			recorder.mu.Lock()
			recorder.last = profile
			recorder.mu.Unlock()
			return entity.TakeFrozenSyncPayload(1, []byte("snapshot")), nil
		},
	}})
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	return recorder
}

// 关系进出：成员集合给了就订阅、拿走就退订、再给又订阅；目标被 Hide 时从关系里消失。目标离观察者很远，只有关系在持有。
func TestRelationMembershipSubscribesAndReleases(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 100, Y: 100}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(subjectState(t, moverID)); err != nil {
		t.Fatal(err)
	}
	if err := interest.Show(moverID, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	setRelation(t, interest, watcherID, moverID)
	mustApply(t, interest)
	requireSessions(t, manager, moverID, entitysync.SessionID(watcherID))
	setRelation(t, interest, watcherID)
	mustApply(t, interest)
	requireSessions(t, manager, moverID)
	setRelation(t, interest, watcherID, moverID)
	mustApply(t, interest)
	requireSessions(t, manager, moverID, entitysync.SessionID(watcherID))
	if err := interest.Hide(moverID); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, moverID)
	// Hide 把它从关系里拿掉了：重新 Show 之后，只有关系的时候看不见它。
	if err := interest.Show(moverID, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, moverID)
}

// 跨来源合并的另一半：关系先撤、距离还在持有，订阅保持；距离也撤了才退订。
func TestDistanceKeepsASubscriptionTheRelationDropped(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	player(t, manager, moverID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, moverID, spatial.Point{X: 520, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	setRelation(t, interest, watcherID, moverID)
	mustApply(t, interest)
	setRelation(t, interest, watcherID)
	mustApply(t, interest)
	if !subscribed(manager, watcherID, moverID) {
		t.Fatal("dropping the relation released a pair distance still holds")
	}
	if err := interest.Move(moverID, spatial.Point{X: 900, Y: 500}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("the last holder went away and the subscription survived")
	}
}

// 分档：目标在同一观察者的距离档之间移动，订阅不断，packer 收到的视图随档变化（缺省映射 band n → bandN）。
func TestDistanceBandsChangeTheSubscribedView(t *testing.T) {
	manager := newManager(t)
	interest, err := NewInterest(InterestConfig{Manager: manager, AOI: AOIConfig{
		Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
		EnterRadius: 120, LeaveRadius: 150, Bands: []int64{40},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	recorder := recordingSubject(t, manager, moverID)
	if err := interest.Show(moverID, spatial.Point{X: 520, Y: 500}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, moverID, entitysync.SessionID(watcherID))
	if got := recorder.profile(); got != DefaultBandProfile(0).Normalize() {
		t.Fatalf("near view = %+v", got)
	}
	if err := interest.Move(moverID, spatial.Point{X: 600, Y: 500}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, moverID, entitysync.SessionID(watcherID))
	if got := recorder.profile(); got != DefaultBandProfile(1).Normalize() {
		t.Fatalf("far view = %+v", got)
	}
}

// self：进场经 self 看见自己；SelfVisible=false 时看不见；Leave 后撤掉。
func TestSelfVisibilityFollowsConfigAndLeave(t *testing.T) {
	for _, visible := range []bool{true, false} {
		manager := newManager(t)
		interest, err := NewInterest(InterestConfig{Manager: manager, SelfVisible: &visible, AOI: AOIConfig{
			Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150,
		}})
		if err != nil {
			t.Fatal(err)
		}
		player(t, manager, watcherID)
		if err := interest.Enter(watcherID, watcherID, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, interest)
		if got := subscribed(manager, watcherID, watcherID); got != visible {
			t.Fatalf("SelfVisible=%v: subscribed to itself = %v", visible, got)
		}
		if err := interest.Leave(watcherID, watcherID); err != nil {
			t.Fatal(err)
		}
		mustApply(t, interest)
		if subscribed(manager, watcherID, watcherID) {
			t.Fatalf("SelfVisible=%v: leaving kept the self subscription", visible)
		}
		interest.Close()
	}
}

// 队伍：三个成员彼此可见（离得再远也一样）；一个成员离队只撤和他有关的 pair；
// 离场（Leave）的成员作为观察者与作为目标两边都被清掉，重新进场后不会凭旧队伍看见任何人。
func TestTeamMembersSeeEachOtherAndLeaveCleansBothSides(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	members := []int64{2001, 2002, 2003}
	for index, id := range members {
		player(t, manager, id)
		if err := interest.Enter(id, id, spatial.Point{X: 100 + int64(index)*400, Y: 100}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
	}
	setTeam := func(team []int64) {
		for _, observer := range team {
			peers := slices.DeleteFunc(slices.Clone(team), func(id int64) bool { return id == observer })
			setRelation(t, interest, observer, peers...)
		}
	}
	setTeam(members)
	mustApply(t, interest)
	for _, subject := range members {
		requireSessions(t, manager, subject, 2001, 2002, 2003)
	}
	// 2003 离队：剩下两人互见，2003 只看见自己、也不再被看见。
	setTeam(members[:2])
	setRelation(t, interest, 2003)
	mustApply(t, interest)
	requireSessions(t, manager, 2001, 2001, 2002)
	requireSessions(t, manager, 2002, 2001, 2002)
	requireSessions(t, manager, 2003, 2003)
	// 2002 离场：两边都清掉；重新进场（远处）后，没有关系让他看见 2001，也没有人看见他。
	if err := interest.Leave(2002, 2002); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, 2001, 2001)
	requireSessions(t, manager, 2002)
	if err := interest.Enter(2002, 2002, spatial.Point{X: 900, Y: 900}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	requireSessions(t, manager, 2001, 2001)
	requireSessions(t, manager, 2002, 2002)
}
