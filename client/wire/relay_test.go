package wire_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// relay 帧不能被 Gate 当成合法包：通道位在 Gate 的 Validate / Read 下仍是保留位。
func TestRelayChannelBitsAreRejectedByTheGateCodec(t *testing.T) {
	for _, flags := range []byte{wire.FlagRelayGame, wire.FlagRelayRoom, wire.FlagRelayControl, wire.FlagRelayGame | wire.FlagPush} {
		h := wire.Header{Flags: flags, MsgID: 7, Seq: 1}
		if err := h.Validate(0); !errors.Is(err, wire.ErrInvalidPacket) {
			t.Fatalf("Gate accepted relay flags %08b: %v", flags, err)
		}
		if err := h.ValidateRelay(0); err != nil {
			t.Fatalf("relay rejected flags %08b: %v", flags, err)
		}
		raw, err := wire.EncodeRelay([]*wire.Packet{{Flags: flags, MsgID: 7, Seq: 1, Payload: []byte{1}}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wire.Read(bytes.NewReader(raw), 0); !errors.Is(err, wire.ErrInvalidPacket) {
			t.Fatalf("Gate Read accepted relay flags %08b: %v", flags, err)
		}
		if _, err := wire.Encode([]*wire.Packet{{Flags: flags, MsgID: 7}}, 0); !errors.Is(err, wire.ErrInvalidPacket) {
			t.Fatalf("Gate Encode accepted relay flags %08b: %v", flags, err)
		}
	}
}

func TestRelayHeaderValidation(t *testing.T) {
	for name, h := range map[string]wire.Header{
		"reserved bit 5":           {Flags: wire.FlagRelayGame | 1<<5, MsgID: 1},
		"sync and lockstep":        {Flags: wire.FlagRelayGame | wire.FlagSync | wire.FlagLockstep, MsgID: 1},
		"control without op":       {Flags: wire.FlagRelayControl},
		"control with push bit":    {Flags: wire.FlagRelayControl | wire.FlagPush, MsgID: wire.RelayOpPing},
		"control with kind bits":   {Flags: wire.FlagRelayControl | wire.FlagSync, MsgID: wire.RelayOpPing},
		"auth frame with push bit": {Flags: wire.FlagPush},
	} {
		if err := h.ValidateRelay(0); !errors.Is(err, wire.ErrInvalidPacket) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	// 数据帧的 message_id 归游戏，可以为 0；推送位与载荷类型原样带过。
	for _, h := range []wire.Header{{Flags: wire.FlagRelayGame}, {Flags: wire.FlagRelayRoom | wire.FlagPush | wire.FlagSync, MsgID: 9}, {MsgID: 0}} {
		if err := h.ValidateRelay(0); err != nil {
			t.Fatalf("%+v rejected: %v", h, err)
		}
	}
	if err := (wire.Header{Flags: wire.FlagRelayGame, PayloadSize: 11}).ValidateRelay(10); !errors.Is(err, wire.ErrPacketTooBig) {
		t.Fatalf("oversized relay frame: %v", err)
	}
}

func TestRelayControlPayloads(t *testing.T) {
	welcome := wire.RelayWelcome{Host: true, HostSlot: 1}
	if got, err := wire.DecodeRelayWelcome(wire.EncodeRelayWelcome(welcome)); err != nil || got != welcome {
		t.Fatalf("welcome %+v %v", got, err)
	}
	if _, err := wire.DecodeRelayWelcome([]byte{3, 0, 0, 0, 1}); !errors.Is(err, wire.ErrInvalidPacket) {
		t.Fatalf("bad role accepted: %v", err)
	}
	member := wire.RelayMember{Reason: wire.RelayReasonKicked, Account: "acc-7"}
	if got, err := wire.DecodeRelayMember(wire.EncodeRelayMember(member)); err != nil || got != member {
		t.Fatalf("member %+v %v", got, err)
	}
	if _, err := wire.DecodeRelayMember([]byte{1}); !errors.Is(err, wire.ErrInvalidPacket) {
		t.Fatalf("member without account accepted: %v", err)
	}
}

// relay 帧的跨语言金样（client/spec/relay-packets.json）。与 packets.json 分开：packets.json 的每一条都要能被
// Gate 的严格解码器（Go、C#）解开，relay 帧不行。C# / TS 的 relay SDK 交付时消费这一份。
func TestRelayProtocolGolden(t *testing.T) {
	type golden struct {
		Name string `json:"name"`
		Hex  string `json:"hex"`
	}
	entries := []struct {
		name   string
		packet *wire.Packet
	}{
		{"relay_token", &wire.Packet{MsgID: 0, Seq: 0, Payload: []byte("rt1.token")}},
		{"relay_game_member_up", &wire.Packet{Flags: wire.FlagRelayGame, MsgID: 101, Seq: wire.RelayAll, Payload: []byte{1, 2}}},
		{"relay_game_to_host", &wire.Packet{Flags: wire.FlagRelayGame, MsgID: 101, Seq: 3, Payload: []byte{1, 2}}},
		{"relay_game_host_to_slot", &wire.Packet{Flags: wire.FlagRelayGame | wire.FlagPush | wire.FlagSync, MsgID: 202, Seq: 3, Payload: []byte{9}}},
		{"relay_room_broadcast", &wire.Packet{Flags: wire.FlagRelayRoom, MsgID: 7, Seq: wire.RelayAll, Payload: []byte("hi")}},
		{"relay_welcome", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpWelcome, Seq: 3, Payload: wire.EncodeRelayWelcome(wire.RelayWelcome{HostSlot: 1})}},
		{"relay_member_joined", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMemberJoined, Seq: 3, Payload: wire.EncodeRelayMember(wire.RelayMember{Account: "a3"})}},
		{"relay_member_left", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMemberLeft, Seq: 3, Payload: wire.EncodeRelayMember(wire.RelayMember{Reason: wire.RelayReasonKicked, Account: "a3"})}},
		{"relay_closed", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpClosed, Payload: []byte{byte(wire.RelayReasonRoomClosed)}}},
		{"relay_muted", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMuted, Seq: 3, Payload: []byte{1}}},
		{"relay_kick", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpKick, Seq: 3}},
		{"relay_mute", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMute, Seq: 3, Payload: []byte{1}}},
		{"relay_ping", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpPing, Payload: []byte{0, 0, 0, 42}}},
		{"relay_pong", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpPong, Payload: []byte{0, 0, 0, 42}}},
	}
	want := []golden{}
	for _, entry := range entries {
		raw, err := wire.EncodeRelay([]*wire.Packet{entry.packet}, 0)
		if err != nil {
			t.Fatalf("%s: %v", entry.name, err)
		}
		decoded, err := wire.DecodeRelay(raw, 0)
		if err != nil || len(decoded) != 1 || decoded[0].Flags != entry.packet.Flags || decoded[0].Seq != entry.packet.Seq || decoded[0].MsgID != entry.packet.MsgID {
			t.Fatalf("%s round trip: %+v %v", entry.name, decoded, err)
		}
		want = append(want, golden{Name: entry.name, Hex: hex.EncodeToString(raw)})
	}
	content, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	path := "../spec/relay-packets.json"
	if *updateGolden {
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	actual = bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(content, actual) {
		t.Fatal("relay golden changed: regenerate deliberately (go test ./client/wire -run TestRelayProtocolGolden -update) and update the SDK consumers")
	}
}
