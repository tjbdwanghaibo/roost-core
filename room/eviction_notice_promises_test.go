package room

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/roost-core/entitysync"
	kit "github.com/tjbdwanghaibo/roost-core/nettransport"
	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

// U-0207 · C8 · RR-20260915-04:已经发生的连接剔除,其通知不能随剩余批次的结果一起丢掉。
// admitWithSlowConsumerPolicy 遇到 ErrReliableBackpressure 时按 SlowConsumerEvict 删传输基线、标记
// deadSessions、RemoveSession 并累积通知,然后用剩余路由重试;重试若再遇普通错误 / 取消,函数
// `return nil, err` 把已累积的通知丢了,调用方 AdmitRoomFrames 也只在整体成功时派发。剔除是不可撤销
// 的副作用,之后的重试对 deadSessions 路由直接跳过,原通知再也生成不出来——room 保留失效订阅和名额,
// 继续为送不到的连接做 profile / 信封工作。承诺:失败批次的 plans / dirty 仍不提交,但已产生的剔除
// 通知在释放 room 锁之后照常派发。

type twoPhaseTransport struct {
	armed  bool
	calls  int
	second error
}

func (t *twoPhaseTransport) AdmitBatch(_ context.Context, _ []kit.OutboundFrame) error {
	if !t.armed {
		return nil
	}
	t.calls++
	switch t.calls {
	case 1:
		return kit.AdmissionError{Session: 1, Err: kit.ErrReliableBackpressure}
	case 2:
		return t.second
	}
	return nil
}

func (t *twoPhaseTransport) RemoveSession(core.SessionID) bool { return true }

func TestEvictionNoticePromiseSurvivesRemainingBatchFailure(t *testing.T) {
	for _, mode := range []string{"success", "transport_error", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			tr := &twoPhaseTransport{}
			switch mode {
			case "transport_error":
				tr.second = errors.New("remaining route rejected")
			case "canceled":
				tr.second = context.Canceled
			}
			sink, err := NewRoomTransportSink(RoomTransportSinkConfig{
				Transport: tr,
				Sessions: RoomSessionResolverFunc(func(_ context.Context, s coreentitysync.SubscriberRef) (core.SessionID, error) {
					return core.SessionID(s.ID), nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer sink.Close(context.Background())
			r, err := NewRoomBroadcaster(7, sink)
			if err != nil {
				t.Fatal(err)
			}
			state := testRoomState(101, nil)
			if err := r.RegisterSubject(state); err != nil {
				t.Fatal(err)
			}
			for id := int64(1); id <= 2; id++ {
				if _, err := r.Subscribe(context.Background(), coreentitysync.SubscriberRef{ID: id}, 101, entity.SyncProfile{}); err != nil {
					t.Fatal(err)
				}
			}
			tr.armed = true
			state.MarkFullDirty(entity.SyncFullReasonResync)
			err = r.FlushDirty(context.Background())
			if mode == "success" && err != nil {
				t.Fatal(err)
			}
			if mode != "success" {
				if err == nil {
					t.Fatal("expected the remaining batch to fail")
				}
				// 失败批次不得提交:状态仍 dirty。
				if !state.PendingDirty() {
					t.Fatal("failed remaining batch consumed the dirty state")
				}
				if err := r.FlushDirty(context.Background()); err != nil {
					t.Fatal("retry:", err)
				}
			}
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				found := false
				for _, s := range r.Subscribers(101) {
					if s.Subscriber.ID == 1 {
						found = true
					}
				}
				if !found {
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatalf("evicted session remains subscribed after retry: sink=%+v room=%+v", sink.Stats(), r.Stats())
		})
	}
}
