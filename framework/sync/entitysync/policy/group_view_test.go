// 组视图（GroupView）：与自身视图装在同一个 Manager 上，会话归 SelfView。成员关系是事实，会话在哪个阶段打开、
// 关闭、重开，成员都在会话打开的那个阶段收到组实体的全量；Leave / Disband 发 remove。

package policy

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// groupRoom 是测试里的组实体（房间）ID，与 selfSubject 映射出的玩家 subject 不相交。
const groupRoom int64 = 5_000_001

var roomProfile = entity.SyncProfile{Key: "room"}

type groupFixture struct {
	*selfFixture
	groups *GroupView
}

// newGroupFixture 按正式接线的顺序装配：先 SelfView（会话），再 GroupView（同一个 Manager，政策阶段排在后面）。
func newGroupFixture(t *testing.T, maxMembers int) *groupFixture {
	t.Helper()
	self := newSelfFixture(t)
	groups, err := NewGroupView(GroupViewConfig{Manager: self.manager, Profile: roomProfile, Lookup: self.memory.lookup, MaxMembers: maxMembers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(groups.Close)
	return &groupFixture{selfFixture: self, groups: groups}
}

// sees 报告这些帧里某个 subject 的最后一次出现：是否存在、是否全量、视图键；remove 之后视为不存在。
func sees(frames []selfFrame, subject int64) (present, full bool, profile string) {
	for _, f := range frames {
		if f.kind == frame.Full {
			present, full, profile = false, false, ""
		}
		for _, object := range f.objects {
			if object.operation == frame.ObjectRemove {
				// remove 不带内容；测试里每帧只关心一个房间，按“这一帧有 remove”处理。
				if object.subject == 0 || object.subject == subject {
					present, full, profile = false, false, ""
				}
				continue
			}
			if object.subject == subject {
				present, full, profile = true, object.full, object.profile
			}
		}
	}
	return present, full, profile
}

func joins(t *testing.T, groups *GroupView, members ...int64) {
	t.Helper()
	for _, member := range members {
		if err := groups.Join(groupRoom, member); err != nil {
			t.Fatalf("join %d: %v", member, err)
		}
	}
}

// 登录前就 Join（会话还没开）：不报错、不丢，SelfView 打开会话的同一个阶段订阅，第一帧全量里自身与房间都在。
// 断线（会话关闭）后成员关系保留；重连的那个阶段重新订阅，房间随新 epoch 的全量回来。期间房间的变化照常推给在线成员。
func TestGroupViewFollowsTheSessionsOfTheSelfView(t *testing.T) {
	f := newGroupFixture(t, 0)
	f.loaded(t, 7)
	f.loaded(t, 8)
	room := subjectState(t, groupRoom)
	f.memory.put(room)
	joins(t, f.groups, 7, 8)
	f.view.Activate(7, "c1")
	f.view.Activate(8, "c1")
	f.flush(t)
	for _, player := range []int64{7, 8} {
		frames := f.lane.take(player)
		requireFullWithRoom(t, frames, player)
	}

	f.view.Deactivate(7, "c1")
	f.flush(t)
	room.MarkDirty(1)
	f.flush(t)
	if frames := f.lane.take(7); len(frames) != 0 {
		t.Fatalf("offline member received %+v", frames)
	}
	if present, full, _ := sees(f.lane.take(8), groupRoom); !present || full {
		t.Fatal("online member did not receive the room delta")
	}
	if f.groups.Members(groupRoom) != 2 {
		t.Fatalf("members after a disconnect = %d, want 2 (membership survives the session)", f.groups.Members(groupRoom))
	}

	f.view.Activate(7, "c2")
	f.flush(t)
	requireFullWithRoom(t, f.lane.take(7), 7)
}

// 顶号：新连接到来时 SelfView Hold/Ready 同一个会话，房间订阅留在会话上，随新 epoch 一起全量重发。
func TestGroupViewRoomIsResentWithTheSecondConnection(t *testing.T) {
	f := newGroupFixture(t, 0)
	f.loaded(t, 7)
	f.memory.put(subjectState(t, groupRoom))
	f.view.Activate(7, "c1")
	f.flush(t)
	_ = f.lane.take(7)
	joins(t, f.groups, 7)
	f.flush(t)
	if present, full, profile := sees(f.lane.take(7), groupRoom); !present || !full || profile != roomProfile.Key {
		t.Fatal("joined member did not receive the room as a full under the room view")
	}
	f.view.Activate(7, "c2")
	f.flush(t)
	requireFullWithRoom(t, f.lane.take(7), 7)
}

// 实体不在内存里（还没加载）时组照样建立，实体一出现（EntityLoaded，或下个阶段的复查）就登记并推给成员。
func TestGroupViewWaitsForTheGroupEntity(t *testing.T) {
	for _, notified := range []bool{true, false} {
		f := newGroupFixture(t, 0)
		f.loaded(t, 7)
		f.view.Activate(7, "c1")
		joins(t, f.groups, 7)
		f.flush(t)
		if present, _, _ := sees(f.lane.take(7), groupRoom); present {
			t.Fatal("room sent before it exists")
		}
		f.memory.put(subjectState(t, groupRoom))
		if notified {
			f.groups.EntityLoaded(groupRoom)
		}
		f.flush(t)
		if present, full, _ := sees(f.lane.take(7), groupRoom); !present || !full {
			t.Fatalf("notified=%v: room not sent once it appeared", notified)
		}
	}
}

// Leave 退订一名成员（它收到 remove，其他成员不受影响）；Disband 退订全部并注销房间；同 ID 再成组是新登记，
// 在旧登记的 remove 交付之后重新送全量。
func TestGroupViewLeaveAndDisband(t *testing.T) {
	f := newGroupFixture(t, 0)
	f.loaded(t, 7)
	f.loaded(t, 8)
	f.memory.put(subjectState(t, groupRoom))
	f.view.Activate(7, "c1")
	f.view.Activate(8, "c1")
	joins(t, f.groups, 7, 8)
	f.flush(t)
	_, _ = f.lane.take(7), f.lane.take(8)

	f.groups.Leave(groupRoom, 8)
	f.flush(t)
	if !removed(f.lane.take(8)) {
		t.Fatal("member that left did not receive a remove")
	}
	if frames := f.lane.take(7); len(frames) != 0 {
		t.Fatalf("remaining member received %+v when another left", frames)
	}

	f.groups.Disband(groupRoom)
	f.flush(t)
	if !removed(f.lane.take(7)) {
		t.Fatal("disband did not remove the room from the last member")
	}
	if f.groups.Groups() != 0 || len(f.manager.Subscribers(groupRoom)) != 0 {
		t.Fatalf("after disband: groups=%d subscribers=%v", f.groups.Groups(), f.manager.Subscribers(groupRoom))
	}
	if err := f.manager.Subscribe(7, groupRoom, roomProfile); !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
		t.Fatalf("subscribe to the disbanded room = %v, want ErrSubjectNotRegistered", err)
	}

	joins(t, f.groups, 7)
	f.flush(t)
	if present, full, _ := sees(f.lane.take(7), groupRoom); !present || !full {
		t.Fatal("a new group with the same id did not send the room again")
	}
}

// Disband 与同 ID 的重新 Join 落在同一个阶段之前：旧登记先退役（remove 交付），新组随后登记，客户端先删后建。
func TestGroupViewDisbandThenRejoinBeforeTheStage(t *testing.T) {
	f := newGroupFixture(t, 0)
	f.loaded(t, 7)
	f.memory.put(subjectState(t, groupRoom))
	f.view.Activate(7, "c1")
	joins(t, f.groups, 7)
	f.flush(t)
	_ = f.lane.take(7)
	f.groups.Disband(groupRoom)
	joins(t, f.groups, 7)
	f.flush(t)
	f.flush(t)
	frames := f.lane.take(7)
	if !removed(frames) {
		t.Fatalf("frames %+v: the old room was not removed before the new group", frames)
	}
	if present, full, _ := sees(frames, groupRoom); !present || !full {
		t.Fatalf("frames %+v: the new group did not send the room as a full after the remove", frames)
	}
}

func TestGroupViewRefusesBeyondTheMemberLimit(t *testing.T) {
	f := newGroupFixture(t, 2)
	joins(t, f.groups, 1, 2)
	if err := f.groups.Join(groupRoom, 3); !errors.Is(err, ErrGroupMemberLimit) {
		t.Fatalf("third member = %v, want ErrGroupMemberLimit", err)
	}
	if err := f.groups.Join(groupRoom, 2); err != nil {
		t.Fatalf("repeated join of a member = %v, want nil", err)
	}
	f.groups.Close()
	if err := f.groups.Join(groupRoom+1, 1); !errors.Is(err, ErrGroupClosed) {
		t.Fatalf("join after Close = %v, want ErrGroupClosed", err)
	}
}

// requireFullWithRoom：第一帧是全量，里面有玩家自己（self 视图）与房间（room 视图），都是全量。
func requireFullWithRoom(t *testing.T, frames []selfFrame, player int64) {
	t.Helper()
	self, _ := selfSubject(player)
	if len(frames) == 0 || frames[0].kind != frame.Full {
		t.Fatalf("player %d frames %+v, want a Full first", player, frames)
	}
	got := map[int64]selfObject{}
	for _, object := range frames[0].objects {
		got[object.subject] = object
	}
	if o := got[self]; !o.full || o.profile != SelfProfile.Key {
		t.Fatalf("player %d first frame %+v lacks its own full self view", player, frames[0])
	}
	if o := got[groupRoom]; !o.full || o.profile != roomProfile.Key {
		t.Fatalf("player %d first frame %+v lacks the full room view", player, frames[0])
	}
}

func removed(frames []selfFrame) bool {
	for _, f := range frames {
		for _, object := range f.objects {
			if object.operation == frame.ObjectRemove {
				return true
			}
		}
	}
	return false
}

// 组同步的容量（KNOWN-LIMITS“组同步的容量与独立 Gate 用例”）：rooms 个组、每组 members 个在线成员，每轮每个组实体
// 变化一次，一次 Flush 把增量推给全部成员（传输层丢弃字节，只量政策 + 打包 + 编码）。报告每轮耗时、每轮字节与每个
// 成员每轮的字节；房间数 / 成员数各取几档，看成本随哪一个增长。测量待机器空闲时跑（-bench GroupView -benchtime 200x）。
func BenchmarkGroupViewFlush(b *testing.B) {
	for _, shape := range []struct{ rooms, members int }{{100, 4}, {1000, 4}, {100, 50}, {10, 500}} {
		b.Run(fmt.Sprintf("rooms=%d/members=%d", shape.rooms, shape.members), func(b *testing.B) {
			mem := &memory{}
			var self atomic.Pointer[SelfView]
			var sent atomic.Int64
			manager, err := entitysync.NewManager(entitysync.ManagerConfig{
				Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, raw []byte) error {
					sent.Add(int64(len(raw)))
					return nil
				}),
				SessionLost: func(session entitysync.SessionID, cause error) {
					if current := self.Load(); current != nil {
						current.SessionLost(session, cause)
					}
				},
			})
			if err != nil {
				b.Fatal(err)
			}
			view, err := NewSelfView(SelfViewConfig{Manager: manager, Subject: selfSubject, Lookup: mem.lookup})
			if err != nil {
				b.Fatal(err)
			}
			self.Store(view)
			groups, err := NewGroupView(GroupViewConfig{Manager: manager, Profile: roomProfile, Lookup: mem.lookup})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				_ = manager.Close(context.Background())
				groups.Close()
				view.Close()
			})
			rooms := make([]*entity.SubjectSyncState, shape.rooms)
			for r := range rooms {
				rooms[r] = subjectState(b, groupRoom+int64(r))
				mem.put(rooms[r])
				for m := range shape.members {
					player := int64(r*shape.members + m + 1)
					subject, _ := selfSubject(player)
					mem.put(subjectState(b, subject))
					if err := groups.Join(groupRoom+int64(r), player); err != nil {
						b.Fatal(err)
					}
					view.Activate(player, "c1")
				}
			}
			ctx := context.Background()
			if err := manager.Flush(ctx); err != nil { // 进场全量不计入
				b.Fatal(err)
			}
			sent.Store(0)
			b.ReportAllocs()
			rounds := 0
			for b.Loop() {
				for _, room := range rooms {
					room.MarkDirty(1)
				}
				if err := manager.Flush(ctx); err != nil {
					b.Fatal(err)
				}
				rounds++
			}
			b.ReportMetric(float64(sent.Load())/float64(rounds), "bytes/round")
			b.ReportMetric(float64(sent.Load())/float64(rounds)/float64(shape.rooms*shape.members), "bytes/member-round")
		})
	}
}
