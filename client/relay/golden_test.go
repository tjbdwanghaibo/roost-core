package relay

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// 跨语言金样：SDK 发出的帧（令牌首帧、游戏 / 房间数据帧、踢人、禁言、心跳）经 SDK 自己的编码路径，字节与
// client/spec/relay-packets.json（client/wire 的 TestRelayProtocolGolden 生成）完全相同；relay 发来的控制帧
// （Welcome、成员上下线、Closed、Muted、Pong）经 SDK 的分发路径解出预期的字段。C#（Roost.Client.Relay.Tests）与
// TS（client/ts/test/golden.test.ts）对同一份金样做同样的核对，三份 SDK 因此对同一帧编出同一组字节。
func TestSDKFramesMatchRelayGolden(t *testing.T) {
	golden := loadGolden(t)
	sent := map[string]Frame{
		"relay_game_member_up":    {Channel: wire.RelayGame, Slot: All, MsgID: 101, Payload: []byte{1, 2}},
		"relay_game_to_host":      {Channel: wire.RelayGame, Slot: 3, MsgID: 101, Payload: []byte{1, 2}},
		"relay_game_host_to_slot": {Channel: wire.RelayGame, Slot: 3, MsgID: 202, Flags: wire.FlagPush | wire.FlagSync, Payload: []byte{9}},
		"relay_room_broadcast":    {Channel: wire.RelayRoom, Slot: All, MsgID: 7, Payload: []byte("hi")},
		"relay_kick":              {Channel: wire.RelayControl, Slot: 3, MsgID: wire.RelayOpKick},
		"relay_mute":              {Channel: wire.RelayControl, Slot: 3, MsgID: wire.RelayOpMute, Payload: []byte{1}},
		"relay_ping":              {Channel: wire.RelayControl, MsgID: wire.RelayOpPing, Payload: []byte{0, 0, 0, 42}},
	}
	for name, frame := range sent {
		data, err := encodeFrame(frame, wire.DefaultMaxPayload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := hex.EncodeToString(data); got != golden[name] {
			t.Fatalf("%s: SDK encoded %s, golden %s", name, got, golden[name])
		}
	}
	token, err := encodeToken("rt1.token")
	if err != nil || hex.EncodeToString(token) != golden["relay_token"] {
		t.Fatalf("token frame %x %v, golden %s", token, err, golden["relay_token"])
	}

	// relay → 客户端：经 dispatch 与内置控制帧处理解出字段。
	c := NewClient(nil, Options{})
	var joined, left []Member
	var muted []bool
	var games, rooms []Frame
	c.OnMemberJoined(func(m Member) { joined = append(joined, m) })
	c.OnMemberLeft(func(m Member) { left = append(left, m) })
	c.OnMuted(func(on bool) { muted = append(muted, on) })
	c.OnGame(func(f Frame) { games = append(games, f) })
	c.OnRoom(func(f Frame) { rooms = append(rooms, f) })
	for _, name := range []string{"relay_welcome", "relay_member_joined", "relay_member_left", "relay_muted", "relay_pong",
		"relay_game_host_to_slot", "relay_room_broadcast"} {
		if reason, err := c.dispatch(decodeGolden(t, golden[name])); reason != wire.RelayReasonNone {
			t.Fatalf("%s: %v %v", name, reason, err)
		}
	}
	if w := c.Welcome(); w != (Welcome{Slot: 3, Host: false, HostSlot: 1}) {
		t.Fatalf("welcome %+v", w)
	}
	if len(joined) != 1 || joined[0] != (Member{Slot: 3, Account: "a3"}) ||
		len(left) != 1 || left[0] != (Member{Slot: 3, Account: "a3", Reason: wire.RelayReasonKicked}) {
		t.Fatalf("members %+v %+v", joined, left)
	}
	if len(muted) != 1 || !muted[0] {
		t.Fatalf("muted %v", muted)
	}
	if len(games) != 1 || games[0].Slot != 3 || games[0].MsgID != 202 || games[0].Flags != wire.FlagPush|wire.FlagSync ||
		len(rooms) != 1 || rooms[0].Slot != All || string(rooms[0].Payload) != "hi" {
		t.Fatalf("data frames %+v %+v", games, rooms)
	}
	reason, err := c.dispatch(decodeGolden(t, golden["relay_closed"]))
	if reason != wire.RelayReasonRoomClosed || err == nil {
		t.Fatalf("closed: %v %v", reason, err)
	}
}

func loadGolden(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile("../spec/relay-packets.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct{ Name, Hex string }
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	golden := map[string]string{}
	for _, entry := range entries {
		golden[entry.Name] = entry.Hex
	}
	// 金样以后会追加（换 host 的 64～127）：这里只要求本 SDK 认识的每一条都在。
	for _, name := range []string{"relay_token", "relay_game_member_up", "relay_game_to_host", "relay_game_host_to_slot", "relay_room_broadcast",
		"relay_welcome", "relay_member_joined", "relay_member_left", "relay_closed", "relay_muted", "relay_kick", "relay_mute", "relay_ping", "relay_pong"} {
		if golden[name] == "" {
			t.Fatalf("relay golden is missing %s", name)
		}
	}
	return golden
}

func decodeGolden(t *testing.T, hexText string) *wire.Packet {
	t.Helper()
	raw, err := hex.DecodeString(hexText)
	if err != nil {
		t.Fatal(err)
	}
	packets, err := wire.DecodeRelay(raw, 0)
	if err != nil || len(packets) != 1 {
		t.Fatalf("decode %s: %v", hexText, err)
	}
	return packets[0]
}

// 不认识的控制帧操作码（例如换 host 留的 64～127，host 组交出编码表之前）丢弃、连接不断；注册了 OnControl 的交给它。
func TestUnknownControlOpIsDroppedOrHandedToOnControl(t *testing.T) {
	c := NewClient(nil, Options{})
	unknown := &wire.Packet{Flags: wire.FlagRelayControl, MsgID: 64, Seq: 2, Payload: []byte{1, 2, 3}}
	if reason, err := c.dispatch(unknown); reason != wire.RelayReasonNone || err != nil {
		t.Fatalf("unknown op ended the connection: %v %v", reason, err)
	}
	var got []Frame
	c.OnControl(64, func(f Frame) { got = append(got, f) })
	if reason, _ := c.dispatch(unknown); reason != wire.RelayReasonNone || len(got) != 1 || got[0].Slot != 2 || len(got[0].Payload) != 3 {
		t.Fatalf("OnControl(64): %v %+v", reason, got)
	}
}
