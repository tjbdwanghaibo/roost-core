package wire_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
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
	welcome := wire.RelayWelcome{Host: true, HostSlot: 1, Epoch: 1<<40 + 3}
	encoded := wire.EncodeRelayWelcome(welcome)
	if got, err := wire.DecodeRelayWelcome(encoded); err != nil || got != welcome || len(encoded) != 13 {
		t.Fatalf("welcome %+v %v (%d bytes)", got, err, len(encoded))
	}
	// 载荷固定 13 字节：旧的 5 字节、多一个字节、角色不是 1 / 2 的都当协议错误。
	bad := append([]byte{3}, encoded[1:]...)
	for _, payload := range [][]byte{bad, encoded[:5], append(append([]byte{}, encoded...), 0)} {
		if _, err := wire.DecodeRelayWelcome(payload); !errors.Is(err, wire.ErrInvalidPacket) {
			t.Fatalf("bad welcome %x accepted: %v", payload, err)
		}
	}
	for _, foreground := range []bool{true, false} {
		if got, err := wire.DecodeRelayPresence(wire.EncodeRelayPresence(foreground)); err != nil || got != foreground {
			t.Fatalf("presence %v: %v %v", foreground, got, err)
		}
	}
	for _, payload := range [][]byte{nil, {2}, {1, 0}} {
		if _, err := wire.DecodeRelayPresence(payload); !errors.Is(err, wire.ErrInvalidPacket) {
			t.Fatalf("bad presence %x accepted: %v", payload, err)
		}
	}
	member := wire.RelayMember{Reason: wire.RelayReasonKicked, Account: "acc-7"}
	if got, err := wire.DecodeRelayMember(wire.EncodeRelayMember(member)); err != nil || got != member {
		t.Fatalf("member %+v %v", got, err)
	}
	if _, err := wire.DecodeRelayMember([]byte{1}); !errors.Is(err, wire.ErrInvalidPacket) {
		t.Fatalf("member without account accepted: %v", err)
	}
}

func TestRelayHostChangePayloads(t *testing.T) {
	if got, err := wire.DecodeRelayEpoch(wire.EncodeRelayEpoch(1<<40 + 5)); err != nil || got != 1<<40+5 {
		t.Fatalf("epoch %d %v", got, err)
	}
	if _, err := wire.DecodeRelayEpoch([]byte{1}); !errors.Is(err, wire.ErrInvalidPacket) {
		t.Fatalf("short epoch accepted: %v", err)
	}
	changed := wire.RelayHostChanged{Epoch: 3, Account: "acc-9"}
	if got, err := wire.DecodeRelayHostChanged(wire.EncodeRelayHostChanged(changed)); err != nil || got != changed {
		t.Fatalf("host changed %+v %v", got, err)
	}
	host := wire.RelayYouAreHost{Epoch: 4, Source: wire.RelayStateSnapshot, SnapshotSeq: 99, SnapshotAgeMillis: 1234,
		Members: []wire.RelaySeat{{Slot: 1, Account: "old-host"}, {Slot: 2, Account: "", Online: true}, {Slot: 5, Account: "acc-5", Online: true}},
		State:   []byte("compressed")}
	got, err := wire.DecodeRelayYouAreHost(wire.EncodeRelayYouAreHost(host))
	if err != nil || got.Epoch != host.Epoch || got.Source != host.Source || got.SnapshotSeq != host.SnapshotSeq ||
		got.SnapshotAgeMillis != host.SnapshotAgeMillis || !slices.Equal(got.Members, host.Members) || !bytes.Equal(got.State, host.State) {
		t.Fatalf("you_are_host %+v %v", got, err)
	}
	encoded := wire.EncodeRelayYouAreHost(host)
	for _, bad := range [][]byte{encoded[:22], encoded[:30], append([]byte{}, encoded[:8]...)} {
		if _, err := wire.DecodeRelayYouAreHost(bad); !errors.Is(err, wire.ErrInvalidPacket) {
			t.Fatalf("truncated you_are_host accepted: %v", err)
		}
	}
	snapshot := wire.RelaySnapshot{Seq: 12, Data: []byte{1, 2, 3}}
	if got, err := wire.DecodeRelaySnapshot(wire.EncodeRelaySnapshot(snapshot)); err != nil || got.Seq != 12 || !bytes.Equal(got.Data, snapshot.Data) {
		t.Fatalf("snapshot %+v %v", got, err)
	}
	transfer := wire.RelayTransfer{Epoch: 1<<33 + 2, Data: []byte{0, 'h'}}
	if got, err := wire.DecodeRelayTransfer(wire.EncodeRelayTransfer(transfer)); err != nil || got.Epoch != transfer.Epoch || !bytes.Equal(got.Data, transfer.Data) {
		t.Fatalf("transfer %+v %v", got, err)
	}
	if _, err := wire.DecodeRelayTransfer(make([]byte, 7)); !errors.Is(err, wire.ErrInvalidPacket) {
		t.Fatalf("transfer without an epoch accepted: %v", err)
	}
	if got, err := wire.DecodeRelaySuccessors(wire.EncodeRelaySuccessors([]uint32{4, 2, 9})); err != nil || !slices.Equal(got, []uint32{4, 2, 9}) {
		t.Fatalf("successors %v %v", got, err)
	}
	if _, err := wire.DecodeRelaySuccessors([]byte{0, 0, 1}); !errors.Is(err, wire.ErrInvalidPacket) {
		t.Fatalf("ragged successors accepted: %v", err)
	}
	if _, err := wire.DecodeRelaySuccessors(make([]byte, 4*(wire.RelayMaxSuccessors+1))); !errors.Is(err, wire.ErrInvalidPacket) {
		t.Fatalf("too many successors accepted: %v", err)
	}
	rejected := wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectTooBig, Seq: 77}
	if got, err := wire.DecodeRelayRejected(wire.EncodeRelayRejected(rejected)); err != nil || got != rejected {
		t.Fatalf("rejected %+v %v", got, err)
	}
	if wire.RelayReasonLobbyRejected.String() != "lobby_rejected" || wire.RelayReasonLobbyUnavailable.String() != "lobby_unavailable" {
		t.Fatal("reason names")
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
		{"relay_welcome", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpWelcome, Seq: 3, Payload: wire.EncodeRelayWelcome(wire.RelayWelcome{HostSlot: 1, Epoch: 5})}},
		{"relay_member_joined", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMemberJoined, Seq: 3, Payload: wire.EncodeRelayMember(wire.RelayMember{Account: "a3"})}},
		{"relay_member_left", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMemberLeft, Seq: 3, Payload: wire.EncodeRelayMember(wire.RelayMember{Reason: wire.RelayReasonKicked, Account: "a3"})}},
		{"relay_closed", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpClosed, Payload: []byte{byte(wire.RelayReasonRoomClosed)}}},
		{"relay_muted", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMuted, Seq: 3, Payload: []byte{1}}},
		{"relay_kick", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpKick, Seq: 3}},
		{"relay_mute", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMute, Seq: 3, Payload: []byte{1}}},
		{"relay_ping", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpPing, Payload: []byte{0, 0, 0, 42}}},
		{"relay_pong", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpPong, Payload: []byte{0, 0, 0, 42}}},
		// 换 host（§4.6）：只追加，不改上面的条目。
		{"relay_you_are_member", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpYouAreMember, Seq: 1, Payload: wire.EncodeRelayEpoch(2)}},
		{"relay_you_are_host_snapshot", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpYouAreHost, Seq: 3, Payload: wire.EncodeRelayYouAreHost(wire.RelayYouAreHost{
			Epoch: 2, Source: wire.RelayStateSnapshot, SnapshotSeq: 7, SnapshotAgeMillis: 1500,
			Members: []wire.RelaySeat{{Slot: 1, Account: "a1"}, {Slot: 3, Account: "a3", Online: true}}, State: []byte{0x00, 0xAB, 0xCD}})}},
		{"relay_you_are_host_handoff", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpYouAreHost, Seq: 2, Payload: wire.EncodeRelayYouAreHost(wire.RelayYouAreHost{
			Epoch: 1, Source: wire.RelayStateHandoff, Members: []wire.RelaySeat{{Slot: 1, Account: "a1", Online: true}, {Slot: 2, Account: "a2", Online: true}}, State: []byte("\x00h")})}},
		{"relay_host_changing", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpHostChanging, Payload: wire.EncodeRelayEpoch(2)}},
		{"relay_host_changed", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpHostChanged, Seq: 3, Payload: wire.EncodeRelayHostChanged(wire.RelayHostChanged{Epoch: 2, Account: "a3"})}},
		{"relay_host_resumed", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpHostResumed, Seq: 1, Payload: wire.EncodeRelayEpoch(0)}},
		{"relay_rejected_snapshot_too_soon", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpRejected, Payload: wire.EncodeRelayRejected(wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectTooSoon, Seq: 8})}},
		{"relay_transfer", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpTransfer, Seq: 3, Payload: wire.EncodeRelayTransfer(wire.RelayTransfer{Epoch: 2, Data: []byte("\x00handoff")})}},
		{"relay_leave", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpLeave}},
		{"relay_successors", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpSuccessors, Payload: wire.EncodeRelaySuccessors([]uint32{3, 2})}},
		{"relay_snapshot", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpSnapshot, Payload: wire.EncodeRelaySnapshot(wire.RelaySnapshot{Seq: 7, Data: []byte{0x00, 0xAB, 0xCD}})}},
		{"relay_host_ready", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpHostReady, Payload: wire.EncodeRelayEpoch(2)}},
		{"relay_closed_lobby_rejected", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpClosed, Payload: []byte{byte(wire.RelayReasonLobbyRejected)}}},
		// 前台上报（2026-11-17）：成员 → relay 的 Presence{前台}，relay → 房主的 MemberPresence{槽位 3，后台}。
		{"relay_presence", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpPresence, Payload: wire.EncodeRelayPresence(true)}},
		{"relay_member_presence", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpMemberPresence, Seq: 3, Payload: wire.EncodeRelayPresence(false)}},
		// 快照字节的压缩方式标识（SDK 之间的约定，relay 不解析）：标识 1 是原始 deflate，后面是一个不压缩的 deflate 块
		// （BFINAL=1、BTYPE=00、LEN=2、NLEN=^2、"hi"），三份 SDK 都解出 "hi"；标识 0x7f 没有分配，三份 SDK 都报“不认识的压缩方式”。
		{"relay_you_are_host_deflate", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpYouAreHost, Seq: 3, Payload: wire.EncodeRelayYouAreHost(wire.RelayYouAreHost{
			Epoch: 2, Source: wire.RelayStateSnapshot, SnapshotSeq: 7, Members: []wire.RelaySeat{{Slot: 3, Account: "a3", Online: true}},
			State: []byte{0x01, 0x01, 0x02, 0x00, 0xFD, 0xFF, 'h', 'i'}})}},
		{"relay_you_are_host_unknown_compression", &wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpYouAreHost, Seq: 3, Payload: wire.EncodeRelayYouAreHost(wire.RelayYouAreHost{
			Epoch: 2, Source: wire.RelayStateSnapshot, SnapshotSeq: 7, Members: []wire.RelaySeat{{Slot: 3, Account: "a3", Online: true}},
			State: []byte{0x7F, 'h', 'i'}})}},
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
