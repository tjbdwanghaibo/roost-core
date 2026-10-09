//go:build integration

package gate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
)

// Room 的唯一 owner 独立于 NATS callback；Input/Attach/Detach/Tick 都进入该作用域。
// 此测试验证原始 LS 协议/生命周期；生成 PB→Nest→DAO/WAL 另由生成消费者覆盖。
func TestRealGateLockstepInputFramesCatchupAndOldDisconnect(t *testing.T) {
	type roomJob struct {
		run  func() error
		done chan error
	}
	jobs := make(chan roomJob, 32)
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		for job := range jobs {
			job.done <- job.run()
		}
	}()
	t.Cleanup(func() { close(jobs); <-ownerDone })
	run := func(fn func() error) error { done := make(chan error, 1); jobs <- roomJob{fn, done}; return <-done }
	var room *lockstep.Room
	var h *gateHarness
	h = realGateHarness(t, func(ctx context.Context, s gateway.Session, id, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		if id != 301 || kind != wire.PayloadLockstep {
			return nil, gateway.ErrInvalidRequest
		}
		command, err := lockstep.DecodeCommand(payload)
		if err != nil {
			return nil, err
		}
		_, receiver, ok := h.game.SessionReceiver(s.Principal().SessionID)
		if !ok {
			return nil, gateway.ErrBindingStale
		}
		return nil, run(func() error {
			if err := room.HandleCommand(nettransport.SessionID(receiver), command); err != nil {
				return err
			}
			_, err := room.Tick(ctx)
			return err
		})
	}, func(deps *gateway.GameDependencies) {
		deps.OnActive = func(_ gateway.Binding, id uint64) error {
			return run(func() error { return room.Attach(123, nettransport.SessionID(id)) })
		}
		deps.OnClosed = func(_ gateway.Binding, id uint64) {
			if err := run(func() error { room.DetachSession(123, nettransport.SessionID(id)); return nil }); err != nil {
				t.Error(err)
			}
		}
	})
	transport := gatewiring.Transport{Game: h.game, LockstepMessageID: 302}
	var err error
	room, err = lockstep.NewRoom(lockstep.RoomConfig{Sequencer: lockstep.SequencerConfig{Players: []lockstep.PlayerID{123}, MaxInputBytes: 16}, RedundancyDepth: 1, Datagrams: transport, Reliable: transport, CatchupSendWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	conn := h.connect(t)
	send := func(seq uint32, command lockstep.Command) {
		t.Helper()
		data, err := lockstep.EncodeCommand(command)
		if err != nil {
			t.Fatal(err)
		}
		if err := wire.Write(conn, []*wire.Packet{{Flags: wire.FlagLockstep, MsgID: 301, Seq: seq, Payload: data}}, h.config.MaxPayloadBytes); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []lockstep.Frame {
		t.Helper()
		packet, err := wire.Read(conn, h.config.MaxPayloadBytes)
		if err != nil || packet.MsgID != 302 || packet.Flags != wire.FlagLockstep|wire.FlagPush {
			t.Fatalf("LS packet=%+v err=%v", packet, err)
		}
		frames, err := lockstep.DecodeBroadcast(packet.Payload)
		if err != nil {
			t.Fatal(err)
		}
		return frames
	}
	send(2, lockstep.Command{Operation: lockstep.OpInput, Frame: 1, Payload: []byte{7}})
	frames := read()
	if len(frames) != 1 || len(frames[0].Inputs) != 1 || frames[0].Inputs[0].Player != 123 || frames[0].Inputs[0].Payload[0] != 7 {
		t.Fatalf("input frame=%+v", frames)
	}
	old, oldReceiver, ok := h.game.SessionReceiver("session")
	if !ok {
		t.Fatal("old receiver absent")
	}
	// 相同认证 SessionID 的重连由 Game 原子替换，旧关闭可能迟到。
	next := h.connect(t)
	conn = next
	send(2, lockstep.Command{Operation: lockstep.OpCatchup, Frame: 1})
	frames = read()
	if len(frames) != 2 || frames[0].ID != 1 || frames[1].ID != 2 {
		t.Fatalf("catchup frames=%+v", frames)
	}
	if err := run(func() error {
		if room.DetachSession(123, nettransport.SessionID(oldReceiver)) {
			return errors.New("late disconnect detached replacement")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.game.PushBound(ctx, old, 303, wire.PayloadLockstep, []byte("stale")); !errors.Is(err, gateway.ErrBindingStale) {
		t.Fatalf("old outbound=%v", err)
	}
	send(3, lockstep.Command{Operation: lockstep.OpInput, Frame: 3, Payload: []byte{9}})
	frames = read()
	if len(frames) != 1 || frames[0].ID != 3 || len(frames[0].Inputs) != 1 || frames[0].Inputs[0].Payload[0] != 9 {
		t.Fatalf("replacement input=%+v", frames)
	}
}
