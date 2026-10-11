package lockstep

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0137 · C2（空洞测试）· nightly gap map core `lockstep` 8/20。
//
// 对局没有数据报发送器不能建；序列器至少一个座位且座位不重复。旁观者会话与
// 座位会话互斥（座位占用的会话不能再当旁观者，旁观者重复挂接幂等）；未挂接的
// 旁观者不能追帧；关闭后的对局拒绝挂接旁观者与哈希上报。`startCatchup` 里的
// closed 检查（match.go:368）在 Close 清空 sessions / spectators 后从两个调用方都
// 到不了（先撞 ErrPlayerDetached），记不可达、保留。

func TestMatchAndSequencerRefuseInvalidConfiguration(t *testing.T) {
	if _, err := NewMatch(MatchConfig{Sequencer: SequencerConfig{Players: []PlayerID{1}}}); !errors.Is(err, ErrMatchConfigInvalid) || !strings.Contains(err.Error(), "datagram sender is required") {
		t.Fatalf("NewMatch without a datagram sender = %v", err)
	}
	if _, err := NewSequencer(SequencerConfig{}); !errors.Is(err, ErrConfigInvalid) || !strings.Contains(err.Error(), "at least one player") {
		t.Fatalf("NewSequencer without players = %v", err)
	}
	if _, err := NewSequencer(SequencerConfig{Players: []PlayerID{1, 2, 1}}); !errors.Is(err, ErrConfigInvalid) || !strings.Contains(err.Error(), "duplicate player 1") {
		t.Fatalf("NewSequencer with a duplicate seat = %v", err)
	}
	if _, err := NewMatch(MatchConfig{Sequencer: SequencerConfig{Players: []PlayerID{1, 1}}, Datagrams: newRecordingTransport()}); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("NewMatch forwards the sequencer's refusal: %v", err)
	}
}

func TestSpectatorSessionsAreExclusiveAndMustBeAttachedToCatchUp(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.AttachSpectator(101); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("a seat's session attached as a spectator: %v", err)
	}
	if _, spectating := match.spectators[101]; spectating {
		t.Fatal("a refused spectator attach still registered the session")
	}
	if err := match.AttachSpectator(500); err != nil {
		t.Fatal(err)
	}
	if err := match.AttachSpectator(500); err != nil {
		t.Fatalf("re-attaching the same spectator session must be idempotent: %v", err)
	}
	if err := match.SpectatorCatchup(501, 1); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("catch-up for a spectator that never attached = %v", err)
	}
	if _, err := match.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := match.SpectatorCatchup(500, 1); err != nil {
		t.Fatalf("catch-up for an attached spectator = %v", err)
	}
}

func TestClosedMatchRefusesSpectatorsAndHashReports(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if _, err := match.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	match.Close()
	if err := match.AttachSpectator(500); !errors.Is(err, ErrMatchClosed) {
		t.Fatalf("AttachSpectator after Close = %v", err)
	}
	if _, spectating := match.spectators[500]; spectating {
		t.Fatal("a closed match registered a spectator")
	}
	if err := match.ReportHash(1, 1, 42); !errors.Is(err, ErrMatchClosed) {
		t.Fatalf("ReportHash after Close = %v", err)
	}
	if err := match.StartCatchup(1, 1); !errors.Is(err, ErrPlayerDetached) {
		t.Fatalf("StartCatchup after Close = %v (Close detaches every seat first)", err)
	}
}
