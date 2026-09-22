package room

import (
	"context"
	"errors"
	"fmt"
	stdsync "sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/roost-core/entitysync"
	kit "github.com/tjbdwanghaibo/roost-core/nettransport"
	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

// U-0277 · C3 · RR-20260922-01：一个断线的观察者不能让房间里其他人再也收不到帧。
//
// 房间把一次 flush 的全部帧按观察者排成一批交给传输层；传输层是原子的，一个会话
// 投不到就整批拒绝。于是"撤掉那个断线的观察者"是唯一的出路——而撤订阅又要先把
// Leave 信封投给它，投不到就恢复 Active。两条规则合起来：它永远在批里，批永远失败，
// 房间里其他人从此一帧都收不到（RR-20260922-01 实跑：16 机器人里排在第一个断线者
// 之后的 13 个会话同一时刻停摆）。
//
// 承诺：Unsubscribe / RetireSubject 对不可达的观察者也要把订阅撤掉，下一次 flush
// 只给还在的人；有不可达订阅者的 subject 照样退役并注销，投不到的 Leave 只是被报出来。

// sessionGoneTransport 是传输层的替身：被标为 gone 的会话让整批被拒绝（原子传输
// 的契约），其余批次原样记下。
type sessionGoneTransport struct {
	mu      stdsync.Mutex
	gone    map[core.SessionID]bool
	batches [][]kit.OutboundFrame
}

func (t *sessionGoneTransport) AdmitBatch(_ context.Context, frames []kit.OutboundFrame) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, frame := range frames {
		if t.gone[frame.Session] {
			return fmt.Errorf("player tcp: session not found: player %d", frame.Session)
		}
	}
	t.batches = append(t.batches, append([]kit.OutboundFrame(nil), frames...))
	return nil
}

func (t *sessionGoneTransport) disconnect(session core.SessionID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gone == nil {
		t.gone = make(map[core.SessionID]bool)
	}
	t.gone[session] = true
}

// framesFor counts the frames a session received after the first `skip` batches
// (the subscription snapshots).
func (t *sessionGoneTransport) framesFor(session core.SessionID, skip int) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for i := skip; i < len(t.batches); i++ {
		for _, frame := range t.batches[i] {
			if frame.Session == session {
				count++
			}
		}
	}
	return count
}

func newRoomWithTwoObservers(t *testing.T, subjectID int64) (*RoomBroadcaster, *sessionGoneTransport, *entity.SubjectSyncState) {
	t.Helper()
	transport := &sessionGoneTransport{}
	sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: transport,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(subscriber.ID), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	room, err := NewRoomBroadcaster(7, sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = room.Close(context.Background()) })
	state := testRoomState(subjectID, nil)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		subscriber := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: id}
		if _, err := room.Subscribe(context.Background(), subscriber, subjectID, entity.SyncProfile{Key: "default"}); err != nil {
			t.Fatalf("subscribe %d: %v", id, err)
		}
	}
	return room, transport, state
}

func TestFlushStillReachesTheOthersAfterAnUnreachableObserverIsUnsubscribed(t *testing.T) {
	room, transport, state := newRoomWithTwoObservers(t, 1001)
	snapshots := len(transport.batches)
	stays := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 1}
	gone := coreentitysync.SubscriberRef{Kind: coreentitysync.SubscriberKindPlayer, ID: 2}

	// 观察者 2 断线，然后场景撤它的订阅——这正是会话关闭钩子做的事。
	transport.disconnect(2)
	_ = room.Unsubscribe(context.Background(), gone, 1001)
	if containsRoomSubscriber(room.Subscribers(1001), gone) {
		t.Fatalf("the departed observer is still a subscriber of subject 1001 after Unsubscribe")
	}

	// 有人改了字段：还在的观察者必须收到这一帧。
	state.MarkDirty(1)
	if err := room.FlushDirty(context.Background()); err != nil {
		t.Fatalf("FlushDirty after the departed observer left: %v", err)
	}
	if got := transport.framesFor(1, snapshots); got != 1 {
		t.Fatalf("observer 1 received %d frame(s) after the flush, want 1: a departed observer starved the room", got)
	}
	if got := transport.framesFor(2, snapshots); got != 0 {
		t.Fatalf("the departed observer still received %d frame(s)", got)
	}
	_ = stays
}

func TestRetireSubjectCompletesWhenASubscriberIsUnreachable(t *testing.T) {
	room, transport, _ := newRoomWithTwoObservers(t, 1002)
	transport.disconnect(2)

	// 退役要完成；那个观察者收不到 Leave 这件事通过返回值说出来，而不是拖住退役。
	err := room.RetireSubject(context.Background(), 1002)
	if !errors.Is(err, coreentitysync.ErrLeaveNotDelivered) {
		t.Fatalf("RetireSubject with an unreachable subscriber: %v, want ErrLeaveNotDelivered", err)
	}
	if got := room.Subscribers(1002); len(got) != 0 {
		t.Fatalf("subscribers remain after retirement: %+v", got)
	}
	// 退役完成的 subject 已经注销：再注销一次只能是"未注册"。
	if err := room.UnregisterSubject(1002); !errors.Is(err, ErrRoomSubjectNotRegistered) {
		t.Fatalf("UnregisterSubject after retirement: %v, want ErrRoomSubjectNotRegistered (the subject was never unregistered)", err)
	}
}
