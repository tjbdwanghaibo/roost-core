package lockstep

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0137 · C2（空洞测试）· nightly gap map core `lockstep` 8/20。
//
// 房间没有数据报发送器不能建；序列器至少一个座位且座位不重复。旁观者会话与
// 座位会话互斥（座位占用的会话不能再当旁观者，旁观者重复挂接幂等）；未挂接的
// 旁观者不能追帧；关闭后的房间拒绝挂接旁观者与哈希上报。`startCatchup` 里的
// closed 检查（room.go:368）在 Close 清空 sessions / spectators 后从两个调用方都
// 到不了（先撞 ErrPlayerDetached），记不可达、保留。

func TestRoomAndSequencerRefuseInvalidConfiguration(t *testing.T) {
	if _, err := NewRoom(RoomConfig{Sequencer: SequencerConfig{Players: []PlayerID{1}}}); !errors.Is(err, ErrRoomConfigInvalid) || !strings.Contains(err.Error(), "datagram sender is required") {
		t.Fatalf("NewRoom without a datagram sender = %v", err)
	}
	if _, err := NewSequencer(SequencerConfig{}); !errors.Is(err, ErrConfigInvalid) || !strings.Contains(err.Error(), "at least one player") {
		t.Fatalf("NewSequencer without players = %v", err)
	}
	if _, err := NewSequencer(SequencerConfig{Players: []PlayerID{1, 2, 1}}); !errors.Is(err, ErrConfigInvalid) || !strings.Contains(err.Error(), "duplicate player 1") {
		t.Fatalf("NewSequencer with a duplicate seat = %v", err)
	}
	if _, err := NewRoom(RoomConfig{Sequencer: SequencerConfig{Players: []PlayerID{1, 1}}, Datagrams: newRecordingTransport()}); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("NewRoom forwards the sequencer's refusal: %v", err)
	}
}

func TestSpectatorSessionsAreExclusiveAndMustBeAttachedToCatchUp(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := room.AttachSpectator(101); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("a seat's session attached as a spectator: %v", err)
	}
	if _, spectating := room.spectators[101]; spectating {
		t.Fatal("a refused spectator attach still registered the session")
	}
	if err := room.AttachSpectator(500); err != nil {
		t.Fatal(err)
	}
	if err := room.AttachSpectator(500); err != nil {
		t.Fatalf("re-attaching the same spectator session must be idempotent: %v", err)
	}
	if err := room.SpectatorCatchup(501, 1); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("catch-up for a spectator that never attached = %v", err)
	}
	if _, err := room.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := room.SpectatorCatchup(500, 1); err != nil {
		t.Fatalf("catch-up for an attached spectator = %v", err)
	}
}

func TestClosedRoomRefusesSpectatorsAndHashReports(t *testing.T) {
	transport := newRecordingTransport()
	room := newTestRoom(t, transport, nil)
	if err := room.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	room.Close()
	if err := room.AttachSpectator(500); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("AttachSpectator after Close = %v", err)
	}
	if _, spectating := room.spectators[500]; spectating {
		t.Fatal("a closed room registered a spectator")
	}
	if err := room.ReportHash(1, 1, 42); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("ReportHash after Close = %v", err)
	}
	if err := room.StartCatchup(1, 1); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("StartCatchup after Close = %v (Close detaches every seat first)", err)
	}
}
