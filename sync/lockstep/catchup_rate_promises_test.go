package lockstep

import (
	"context"
	"errors"
	"testing"
)

// U-0194 · C8 · RR-20260914-05:被接受的追帧配置必须能在正常链路下追上帧头并切回 live。
// 旧实现接受 CatchupBatchFrames=1:每 Tick 先产一帧再补至多一帧,积压 +1-1=0 永不收敛——
// 客户端持续收到固定落后的历史,CatchingUp 不结束,live datagram 一直被跳过。承诺:补帧净速度
// 必须大于产帧速度,batch=1 在创建时拒绝;batch=2 从落后 5 帧起有限轮内收敛。

func TestRoomPromiseCatchupBatchMustOutpaceFrameProduction(t *testing.T) {
	for _, batch := range []int{1, 2} {
		t.Run(string(rune('0'+batch)), func(t *testing.T) {
			tr := newRecordingTransport()
			r, err := NewRoom(RoomConfig{
				Sequencer:          SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8},
				CatchupBatchFrames: batch, Datagrams: tr, Reliable: tr,
			})
			if err != nil {
				if batch == 1 && errors.Is(err, ErrRoomConfigInvalid) {
					return // refusing the non-converging configuration is the contract
				}
				t.Fatal(err)
			}
			defer r.Close()
			ctx := context.Background()
			for i := 0; i < 5; i++ {
				if _, err := r.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Attach(1, 101); err != nil {
				t.Fatal(err)
			}
			if err := r.StartCatchup(1, 1); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 20; i++ {
				if _, err := r.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if r.CatchingUp(1) {
				t.Fatalf("batch=%d never closes backlog: next=%d head=%d reliable_pages=%d",
					batch, r.catchups[101].next, r.history.Latest(), len(tr.reliable[101]))
			}
		})
	}
}
