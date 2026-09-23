package policy

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/entitysync"
	"github.com/tjbdwanghaibo/roost-core/spatial"
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

func newInterest(t *testing.T, manager *entitysync.Manager, relations ...string) *Interest {
	t.Helper()
	interest, err := NewInterest(InterestConfig{
		Manager: manager,
		Spatial: spatial.InterestConfig{
			Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
			EnterRadius: 120, LeaveRadius: 150,
		},
		Relations: relations,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	return interest
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

// 进场就订阅自己——经 self 关系源，不经距离：spatial 不让任何东西观察自己。
func TestEnteringSubscribesToYourself(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager)
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, spatial.Point{X: 500, Y: 500}); err != nil {
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
	if err := interest.Enter(watcherID, spatial.Point{X: 500, Y: 500}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, spatial.Point{X: 500 + 119, Y: 500}); err != nil {
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

// 关系源比距离活得久：队友走出视野不撤订阅，退队之后（最后一个来源）才撤。
func TestARelationKeepsASubscriptionDistanceDropped(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager, "team")
	player(t, manager, watcherID)
	player(t, manager, moverID)
	if err := interest.Enter(watcherID, spatial.Point{X: 500, Y: 500}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, spatial.Point{X: 520, Y: 500}); err != nil {
		t.Fatal(err)
	}
	interest.Relation("team").Set(watcherID, []int64{moverID})
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
	interest.Relation("team").Set(watcherID, nil)
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) {
		t.Fatal("the last source went away and the subscription survived")
	}
}

// 离场双向释放，调用方不用记得谁在看。
func TestLeavingReleasesBothDirections(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager, "team")
	player(t, manager, watcherID)
	player(t, manager, moverID)
	if err := interest.Enter(watcherID, spatial.Point{X: 500, Y: 500}); err != nil {
		t.Fatal(err)
	}
	if err := interest.Enter(moverID, spatial.Point{X: 520, Y: 500}); err != nil {
		t.Fatal(err)
	}
	interest.Relation("team").Set(watcherID, []int64{moverID})
	mustApply(t, interest)
	if err := interest.Leave(moverID); err != nil {
		t.Fatal(err)
	}
	mustApply(t, interest)
	if subscribed(manager, watcherID, moverID) || subscribed(manager, moverID, moverID) || subscribed(manager, moverID, watcherID) {
		t.Fatalf("a leaver is still held somewhere: %v / %v", manager.Subscribers(moverID), manager.Subscribers(watcherID))
	}
}

// 被拒的 subscribe 在每次 Apply 再说一次，直到被接受或 pair 被释放（RR-20260920-06）：
// 队伍在队友入场之前就成了，第一次订阅因 subject 未注册被拒，队友入场后的 Apply 补上。
func TestARefusedSubscribeIsSaidAgainUntilItIsTaken(t *testing.T) {
	manager := newManager(t)
	interest := newInterest(t, manager, "team")
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, spatial.Point{X: 100, Y: 100}); err != nil {
		t.Fatal(err)
	}
	// The teammate has not joined: no subject, no session.
	interest.Relation("team").Set(watcherID, []int64{moverID})
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
	if err := interest.Enter(moverID, spatial.Point{X: 900, Y: 900}); err != nil {
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
	interest := newInterest(t, manager, "team")
	player(t, manager, watcherID)
	if err := interest.Enter(watcherID, spatial.Point{X: 100, Y: 100}); err != nil {
		t.Fatal(err)
	}
	interest.Relation("team").Set(watcherID, []int64{moverID})
	if refusals := interest.Apply(); len(refusals) != 1 {
		t.Fatalf("refusals = %+v", refusals)
	}
	interest.Relation("team").Set(watcherID, nil)
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
	if manager.Stats().Subjects != 0 {
		t.Fatalf("closing the group left subjects registered: %+v", manager.Stats())
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
