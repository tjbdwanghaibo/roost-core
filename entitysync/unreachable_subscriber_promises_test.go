package entitysync

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0277 · C3 · RR-20260922-01：撤订阅不能以"通知到对方"为前提。
//
// Unsubscribe 先把订阅置为 Closing，再把一条 Leave 信封投给被撤的观察者，
// 投递失败就 restoreActive——把"没能通知它"当成"没能撤掉它"。对一个会话已经
// 不在的观察者（实跑里是跑完脚本断线的客户端），投递永远失败，它的订阅就永远
// Active：房间每次 flush 都为它生成帧，生成工程的批量传输在它那里失败、整批放弃，
// 排在它后面的所有观察者从此收不到任何帧（16 机器人里 6 个 `pos_x` 永不到达）。
//
// 新承诺：Unsubscribe 之后订阅一定不在了；Leave 信封尽力投递，投不到把原因
// 报回调用方，但不改变"已撤掉"这个结果。
func TestUnsubscribeRemovesTheSubscriptionEvenWhenTheLeaveCannotBeDelivered(t *testing.T) {
	sink := &recordingEnvelopeSink{}
	coordinator := NewSubscriptionCoordinator(sink)
	t.Cleanup(coordinator.Close)
	packCount := 0
	state := newSubscriptionTestState(t, 2201, &packCount)
	stays := SubscriberRef{Kind: SubscriberKindPlayer, ID: 1}
	gone := SubscriberRef{Kind: SubscriberKindPlayer, ID: 2}
	for _, subscriber := range []SubscriberRef{stays, gone} {
		if _, err := coordinator.Subscribe(context.Background(), subscriber, state, entity.SyncProfile{Key: "default"}); err != nil {
			t.Fatalf("subscribe %d: %v", subscriber.ID, err)
		}
	}

	// 观察者 2 的连接没了：从这里起给它的任何投递都失败。
	cause := errors.New("player tcp: session not found: player 2")
	sink.rejectErr = cause
	err := coordinator.Unsubscribe(context.Background(), gone, state.SubjectID())
	if !errors.Is(err, cause) || !errors.Is(err, ErrLeaveNotDelivered) {
		t.Fatalf("Unsubscribe error=%v, want ErrLeaveNotDelivered wrapping %v", err, cause)
	}
	if got, ok := coordinator.Get(gone, state.SubjectID()); ok {
		t.Fatalf("an unreachable observer is still subscribed after Unsubscribe: %+v", got)
	}

	// 下一次 flush 只该给还在的观察者。
	sink.rejectErr = nil
	state.MarkDirty(1)
	if err := coordinator.FlushSubject(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	batches := sink.snapshot()
	last := batches[len(batches)-1]
	if len(last) != 1 || last[0].Subscriber != stays {
		t.Fatalf("flush after Unsubscribe produced envelopes %+v, want exactly one for observer %d", last, stays.ID)
	}
}
