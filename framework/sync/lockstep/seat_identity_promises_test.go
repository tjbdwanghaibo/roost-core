package lockstep

import (
	"context"
	"errors"
	"testing"
)

// U-0195 · C8 · RR-20260914-06:座位号与旁观者标记不能共用一个值域。Room 用 PlayerID(-1) 当
// sessionOwners 里的旁观者哨兵,而 Sequencer 接受 Players={-1}:Attach(-1, 101) 之后
// AttachSpectator(101) 仍成功,同一 session 同时在 seats 与 spectators 里,broadcastOrder 列两次,
// DetachSpectator 会清掉座位的 catchup。承诺:座位号必须非负,负值在创建时拒绝。
func TestRoomPromiseNegativeSeatIsRefused(t *testing.T) {
	tr := newRecordingTransport()
	r, err := NewRoom(RoomConfig{Sequencer: SequencerConfig{Players: []PlayerID{-1}, MaxInputBytes: 8}, Datagrams: tr, Reliable: tr})
	if err != nil {
		if !errors.Is(err, ErrConfigInvalid) {
			t.Fatalf("negative seat refused with the wrong error: %v", err)
		}
		return
	}
	defer r.Close()
	if err := r.Attach(-1, 101); err != nil {
		t.Fatal(err)
	}
	if err := r.AttachSpectator(101); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("same session accepted as seat -1 and spectator: %v", err)
	}
}

// U-0196 · C4 · RR-20260914-07:被接受的房间不能生成自己的解码器拒绝的帧。NewRoom 只按字节预算
// 校验(257 座位 × 1 字节 × 深度 1 放得进 8192B 的 datagram),没有对齐 wire 的 MaxFrameInputs=256;
// 全员提交后 Tick 成功,DecodeBroadcast 报 `input count 257`。承诺:座位数在创建时对齐协议上限。
func TestRoomPromiseSeatCountIsBoundByTheWireDecoder(t *testing.T) {
	tr := newRecordingTransport()
	players := make([]PlayerID, MaxFrameInputs+1)
	for i := range players {
		players[i] = PlayerID(i + 1)
	}
	r, err := NewRoom(RoomConfig{Sequencer: SequencerConfig{Players: players, MaxInputBytes: 1}, RedundancyDepth: 1, MaxDatagramBytes: 8192, Datagrams: tr})
	if err != nil {
		if !errors.Is(err, ErrConfigInvalid) {
			t.Fatalf("oversized seat set refused with the wrong error: %v", err)
		}
	} else {
		defer r.Close()
		if err := r.Attach(1, 101); err != nil {
			t.Fatal(err)
		}
		for _, p := range players {
			if _, err := r.SubmitInput(p, 1, []byte{1}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeBroadcast(tr.datagrams[101][0]); err != nil {
			t.Fatalf("accepted room emits undecodable frame: %v", err)
		}
	}
	// 对照:刚好 MaxFrameInputs 个座位可以建房,且全员输入的帧可解码。
	full := players[:MaxFrameInputs]
	r2, err := NewRoom(RoomConfig{Sequencer: SequencerConfig{Players: full, MaxInputBytes: 1}, RedundancyDepth: 1, MaxDatagramBytes: 8192, Datagrams: tr})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if err := r2.Attach(1, 102); err != nil {
		t.Fatal(err)
	}
	for _, p := range full {
		if _, err := r2.SubmitInput(p, 1, []byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if frames, err := DecodeBroadcast(tr.datagrams[102][0]); err != nil || len(frames[0].Inputs) != MaxFrameInputs {
		t.Fatalf("full room frame: %v inputs=%d", err, len(frames[0].Inputs))
	}
}
