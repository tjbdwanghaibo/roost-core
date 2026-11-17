package relay

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
	if w := c.Welcome(); w != (Welcome{Slot: 3, Host: false, HostSlot: 1, Epoch: 5}) {
		t.Fatalf("welcome %+v", w)
	}
	if r := c.Role(); r.Epoch != 5 {
		t.Fatalf("role after welcome %+v: epoch comes from the Welcome", r)
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
	// 金样以后可能追加：这里只要求本 SDK 认识的每一条都在。
	for _, name := range []string{"relay_token", "relay_game_member_up", "relay_game_to_host", "relay_game_host_to_slot", "relay_room_broadcast",
		"relay_welcome", "relay_member_joined", "relay_member_left", "relay_closed", "relay_muted", "relay_kick", "relay_mute", "relay_ping", "relay_pong",
		"relay_you_are_member", "relay_you_are_host_snapshot", "relay_you_are_host_handoff", "relay_host_changing", "relay_host_changed",
		"relay_host_resumed", "relay_rejected_snapshot_too_soon", "relay_transfer", "relay_leave", "relay_successors", "relay_snapshot",
		"relay_host_ready", "relay_closed_lobby_rejected", "relay_presence", "relay_member_presence", "relay_you_are_host_deflate",
		"relay_you_are_host_unknown_compression"} {
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

// 不认识的控制帧操作码（127 没有分配）丢弃、连接不断；注册了 OnControl 的交给它。
func TestUnknownControlOpIsDroppedOrHandedToOnControl(t *testing.T) {
	c := NewClient(nil, Options{})
	unknown := &wire.Packet{Flags: wire.FlagRelayControl, MsgID: 127, Seq: 2, Payload: []byte{1, 2, 3}}
	if reason, err := c.dispatch(unknown); reason != wire.RelayReasonNone || err != nil {
		t.Fatalf("unknown op ended the connection: %v %v", reason, err)
	}
	var got []Frame
	c.OnControl(127, func(f Frame) { got = append(got, f) })
	if reason, _ := c.dispatch(unknown); reason != wire.RelayReasonNone || len(got) != 1 || got[0].Slot != 2 || len(got[0].Payload) != 3 {
		t.Fatalf("OnControl(127): %v %+v", reason, got)
	}
}

// 换 host 的 13 条金样：
//   - SDK 发出的（Transfer、Leave、SetSuccessors、Snapshot、HostReady）经 SDK 的公开方法写到连接上，字节与金样相同
//     （Compressor 用 NoCompression，金样里的交接数据与快照是原始字节；Snapshot 的 seq 由 SDK 递增）；
//   - relay 发来的（YouAreMember、两种 YouAreHost、HostChanging、HostChanged、HostResumed、Rejected、Closed{lobby_rejected}）
//     经 dispatch 解出预期的结构，角色按控制帧更新。
func TestSDKHostFramesMatchRelayGolden(t *testing.T) {
	golden := loadGolden(t)
	c := NewClient(nil, Options{Compressor: NoCompression, SnapshotMinInterval: time.Nanosecond})
	local, remote := net.Pipe()
	defer remote.Close()
	sess := &session{netConn: local, stop: make(chan struct{}), wake: make(chan struct{}, 1)}
	c.mu.Lock()
	c.current, c.role = sess, Welcome{Slot: 1, Host: true, HostSlot: 1}
	c.host.snapSeq = 6 // 下一份快照的 seq 是 7
	c.mu.Unlock()
	expect := func(name string, send func() error) {
		t.Helper()
		want, _ := hex.DecodeString(golden[name])
		errc := make(chan error, 1)
		go func() { errc <- send() }()
		got := make([]byte, len(want))
		_ = remote.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(remote, got); err != nil {
			t.Fatalf("%s: read %v", name, err)
		}
		if err := <-errc; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if hex.EncodeToString(got) != golden[name] {
			t.Fatalf("%s: SDK sent %x, golden %s", name, got, golden[name])
		}
	}
	expect("relay_transfer", func() error { return c.Transfer(3, []byte("handoff")) })
	expect("relay_successors", func() error { return c.SetSuccessors([]uint32{3, 2}) })
	expect("relay_snapshot", func() error { return c.Snapshot([]byte{0xAB, 0xCD}) })
	expect("relay_host_ready", func() error { return c.HostReady(2) })
	expect("relay_presence", func() error { return c.SetForeground(true) })
	expect("relay_leave", func() error { _, err := c.Leave(context.Background()); return err })

	// relay → 客户端。
	d := NewClient(nil, Options{Compressor: NoCompression})
	var offers []YouAreHost
	var members, changing []uint64
	var changed, resumed []HostEvent
	var rejected []Rejected
	d.OnYouAreHost(func(e YouAreHost) { offers = append(offers, e) })
	d.OnYouAreMember(func(epoch uint64) { members = append(members, epoch) })
	d.OnHostChanging(func(epoch uint64) { changing = append(changing, epoch) })
	d.OnHostChanged(func(e HostEvent) { changed = append(changed, e) })
	d.OnHostResumed(func(e HostEvent) { resumed = append(resumed, e) })
	d.OnRejected(func(r Rejected) { rejected = append(rejected, r) })
	var presence []MemberPresence
	d.OnMemberPresence(func(p MemberPresence) { presence = append(presence, p) })
	step := func(name string, want Role) {
		t.Helper()
		if reason, err := d.dispatch(decodeGolden(t, golden[name])); reason != wire.RelayReasonNone {
			t.Fatalf("%s: %v %v", name, reason, err)
		}
		if got := d.Role(); got != want {
			t.Fatalf("after %s role %+v, want %+v", name, got, want)
		}
	}
	step("relay_welcome", Role{Slot: 3, HostSlot: 1, Epoch: 5})
	step("relay_member_presence", Role{Slot: 3, HostSlot: 1, Epoch: 5})
	step("relay_host_resumed", Role{Slot: 3, HostSlot: 1})
	step("relay_host_changing", Role{Slot: 3, HostSlot: 1})
	step("relay_you_are_host_snapshot", Role{Slot: 3, Candidate: true, Epoch: 2})
	step("relay_host_changed", Role{Slot: 3, Host: true, HostSlot: 3, Epoch: 2})
	step("relay_rejected_snapshot_too_soon", Role{Slot: 3, Host: true, HostSlot: 3, Epoch: 2})
	step("relay_you_are_host_handoff", Role{Slot: 2, Candidate: true, Epoch: 1})
	step("relay_you_are_member", Role{Slot: 2, Epoch: 2})
	if len(resumed) != 1 || resumed[0] != (HostEvent{Epoch: 0, HostSlot: 1}) ||
		len(changing) != 1 || changing[0] != 2 ||
		len(changed) != 1 || changed[0] != (HostEvent{Epoch: 2, HostSlot: 3, Account: "a3"}) ||
		len(rejected) != 1 || rejected[0] != (Rejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectTooSoon, Seq: 8}) ||
		len(members) != 1 || members[0] != 2 {
		t.Fatalf("resumed %+v changing %v changed %+v rejected %+v members %v", resumed, changing, changed, rejected, members)
	}
	if len(offers) != 2 {
		t.Fatalf("offers %+v", offers)
	}
	if len(presence) != 1 || presence[0] != (MemberPresence{Slot: 3, Foreground: false}) {
		t.Fatalf("member presence %+v", presence)
	}
	snap, hand := offers[0], offers[1]
	if snap.Epoch != 2 || snap.Source != StateSnapshot || snap.SnapshotSeq != 7 || snap.SnapshotAge != 1500*time.Millisecond ||
		len(snap.Members) != 2 || snap.Members[0] != (Seat{Slot: 1, Account: "a1"}) || snap.Members[1] != (Seat{Slot: 3, Account: "a3", Online: true}) ||
		string(snap.State) != "\xab\xcd" || snap.StateErr != nil {
		t.Fatalf("snapshot offer %+v", snap)
	}
	if hand.Epoch != 1 || hand.Source != StateHandoff || hand.SnapshotSeq != 0 || len(hand.Members) != 2 || !hand.Members[0].Online ||
		string(hand.State) != "h" {
		t.Fatalf("handoff offer %+v", hand)
	}
	reason, err := d.dispatch(decodeGolden(t, golden["relay_closed_lobby_rejected"]))
	if reason != wire.RelayReasonLobbyRejected || err == nil {
		t.Fatalf("closed lobby_rejected: %v %v", reason, err)
	}

	// 压缩方式标识：标识 1（原始 deflate）的状态字节哪种 Compressor 的客户端都解得开（内置的两种总认识）；
	// 标识 0x7f 没有分配，State 为 nil、StateErr 是 ErrUnknownCompression。
	offers = nil
	for _, name := range []string{"relay_you_are_host_deflate", "relay_you_are_host_unknown_compression"} {
		if reason, err := d.dispatch(decodeGolden(t, golden[name])); reason != wire.RelayReasonNone {
			t.Fatalf("%s: %v %v", name, reason, err)
		}
	}
	if len(offers) != 2 || string(offers[0].State) != "hi" || offers[0].StateErr != nil {
		t.Fatalf("deflate-marked offer %+v", offers)
	}
	if offers[1].State != nil || !errors.Is(offers[1].StateErr, ErrUnknownCompression) {
		t.Fatalf("unknown-marked offer: state %q err %v", offers[1].State, offers[1].StateErr)
	}
}

// 快照 seq 的接续：新房主从 YouAreHost 里 relay 存着的 seq 往上接；主动转交的 YouAreHost 不带 seq，就从 epoch<<32 往上接
// （relay 可能还存着旧房主更大的 seq）。
func TestSnapshotSeqContinuesAcrossHosts(t *testing.T) {
	c := NewClient(nil, Options{Compressor: NoCompression})
	offer := func(epoch, seq uint64, source wire.RelayStateSource) {
		t.Helper()
		payload := wire.EncodeRelayYouAreHost(wire.RelayYouAreHost{Epoch: epoch, Source: source, SnapshotSeq: seq})
		if reason, err := c.dispatch(&wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpYouAreHost, Seq: 2, Payload: payload}); reason != wire.RelayReasonNone {
			t.Fatal(reason, err)
		}
	}
	nextSeq := func() uint64 { c.mu.Lock(); defer c.mu.Unlock(); return c.host.snapSeq + 1 }
	offer(1, 1<<40, StateSnapshot) // 前一个房主（别的 SDK）用了很大的 seq
	if got := nextSeq(); got != 1<<40+1 {
		t.Fatalf("after a snapshot offer next seq %d, want %d", got, uint64(1<<40+1))
	}
	c = NewClient(nil, Options{Compressor: NoCompression})
	offer(3, 0, StateHandoff)
	if got := nextSeq(); got != 3<<32+1 {
		t.Fatalf("after a handoff offer next seq %d, want %d", got, uint64(3<<32+1))
	}
}

// 快照字节的压缩方式标识（SDK 之间的约定，RELAY.md §2.3）：发出的非空状态以 Compressor.ID() 开头，空状态不带标识；
// 收到时按标识选解压方式——自己的 Compressor 或内置的两种。标识不认识返回 ErrUnknownCompression；标识对、内容坏返回
// 解压错误（不是 ErrUnknownCompression）。房间里混用了游戏自带的压缩方式时，新房主据此得到明确的错误。
func TestStateCarriesTheCompressionMarker(t *testing.T) {
	state := bytes.Repeat([]byte("seed=42 "), 64)
	offer := func(receiver Compressor, data []byte) YouAreHost {
		t.Helper()
		c := NewClient(nil, Options{Compressor: receiver})
		var got []YouAreHost
		c.OnYouAreHost(func(e YouAreHost) { got = append(got, e) })
		payload := wire.EncodeRelayYouAreHost(wire.RelayYouAreHost{Epoch: 1, Source: StateHandoff, State: data})
		if reason, err := c.dispatch(&wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpYouAreHost, Seq: 2, Payload: payload}); reason != wire.RelayReasonNone {
			t.Fatal(reason, err)
		}
		return got[0]
	}
	packed := func(sender Compressor, data []byte) []byte {
		t.Helper()
		c := NewClient(nil, Options{Compressor: sender})
		out, err := c.compressState(data)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	deflated := packed(Deflate, state)
	if deflated[0] != 1 || packed(NoCompression, []byte("ab"))[0] != 0 || string(packed(NoCompression, []byte("ab"))[1:]) != "ab" {
		t.Fatalf("markers: deflate %x, none %x", deflated[:1], packed(NoCompression, []byte("ab")))
	}
	if empty := packed(Deflate, nil); len(empty) != 0 {
		t.Fatalf("empty state packed to %x, want no bytes", empty)
	}
	custom := markerCompressor{id: 9}
	for _, tc := range []struct {
		name     string
		receiver Compressor
		data     []byte
	}{
		{"deflate → deflate", Deflate, deflated},
		{"deflate → custom (built-in fallback)", custom, deflated},
		{"none → deflate", Deflate, packed(NoCompression, state)},
		{"custom → custom", custom, packed(custom, state)},
	} {
		if got := offer(tc.receiver, tc.data); got.StateErr != nil || !bytes.Equal(got.State, state) {
			t.Fatalf("%s: state %d bytes, err %v", tc.name, len(got.State), got.StateErr)
		}
	}
	if got := offer(Deflate, nil); got.StateErr != nil || len(got.State) != 0 {
		t.Fatalf("empty state: %q %v", got.State, got.StateErr)
	}
	// 混用：发的人用游戏自带的 9 号，收的人只认 deflate 与不压缩。
	if got := offer(Deflate, packed(custom, state)); got.State != nil || !errors.Is(got.StateErr, ErrUnknownCompression) ||
		!strings.Contains(got.StateErr.Error(), "9") {
		t.Fatalf("mixed compressors: state %q err %v", got.State, got.StateErr)
	}
	// 标识对、内容坏：解压错误，不是“不认识”。
	broken := append([]byte{1}, 0xFF, 0xFF, 0xFF)
	if got := offer(Deflate, broken); got.State != nil || got.StateErr == nil || errors.Is(got.StateErr, ErrUnknownCompression) {
		t.Fatalf("corrupt deflate: state %q err %v", got.State, got.StateErr)
	}
}

// markerCompressor 是游戏自带压缩方式的替身：不改字节，只换标识。
type markerCompressor struct{ id byte }

func (m markerCompressor) ID() byte                           { return m.id }
func (markerCompressor) Compress(data []byte) ([]byte, error) { return data, nil }
func (markerCompressor) Decompress(data []byte, limit int) ([]byte, error) {
	return NoCompression.Decompress(data, limit)
}

// Deflate 与 NoCompression：压缩后解压得回原样；解压超过上限的报错（防解压炸弹）。
func TestCompressors(t *testing.T) {
	state := bytes.Repeat([]byte("seed=42 deck=[7,1,9] "), 200)
	for _, comp := range []Compressor{Deflate, NoCompression} {
		packed, err := comp.Compress(state)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := comp.Decompress(packed, len(state)); err != nil || string(out) != string(state) {
			t.Fatalf("%T round trip %q %v", comp, out, err)
		}
		if _, err := comp.Decompress(packed, len(state)-1); err == nil {
			t.Fatalf("%T decompressed past the limit", comp)
		}
	}
	if packed, _ := Deflate.Compress(state); len(packed) >= len(state) {
		t.Fatalf("deflate did not shrink a repetitive state (%d → %d)", len(state), len(packed))
	}
}

// Deflate 复用压缩器与解压器（2026-11-18）：
//   - 线上字节不变：一串大小、内容各不相同的状态，复用的压缩器压出的字节与每次新建的 flate.Writer 逐字节相同（前一次的
//     状态不漏到下一次）；
//   - 解压器在解坏数据、超过上限之后照常解下一份；
//   - 并发压缩 / 解压得回原样（-race 下跑）；
//   - 每次压缩的平均分配远小于新建一个 flate.Writer（约 1 MB）：1KiB 状态压 200 次，平均不到 512 KiB（-race 下 sync.Pool
//     随机丢掉四分之一的 Put，平均仍在这条线以下）。
func TestDeflateReuse(t *testing.T) {
	fresh := func(data []byte) []byte {
		var out bytes.Buffer
		w, err := flate.NewWriter(&out, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
		_ = w.Close()
		return out.Bytes()
	}
	var states [][]byte
	for i, size := range []int{1, 300, 70 << 10, 0, 5 << 10, 256 << 10, 2 << 10} {
		state := make([]byte, size)
		for j := range state {
			state[j] = byte((j*7 + i*13) % 61) // 有重复、可压
		}
		if i%2 == 1 {
			copy(state, bytes.Repeat([]byte{byte(i)}, size/2))
		}
		states = append(states, state)
	}
	for round := 0; round < 3; round++ {
		for i, state := range states {
			packed, err := Deflate.Compress(state)
			if err != nil {
				t.Fatal(err)
			}
			if want := fresh(state); !bytes.Equal(packed, want) {
				t.Fatalf("round %d state %d (%d bytes): reused compressor wrote %d bytes, a fresh flate.Writer %d", round, i, len(state), len(packed), len(want))
			}
			if _, err := Deflate.Decompress(packed[:len(packed)/2], MaxStateBytes); err == nil && len(state) > 300 {
				t.Fatalf("state %d: a truncated stream decompressed", i)
			}
			if len(state) > 1 {
				if _, err := Deflate.Decompress(packed, len(state)-1); err == nil {
					t.Fatalf("state %d: decompressed past the limit", i)
				}
			}
			if out, err := Deflate.Decompress(packed, MaxStateBytes); err != nil || !bytes.Equal(out, state) {
				t.Fatalf("round %d state %d: round trip after failures: %d bytes, %v", round, i, len(out), err)
			}
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				state := states[(g+i)%len(states)]
				packed, err := Deflate.Compress(state)
				if err == nil {
					var out []byte
					if out, err = Deflate.Decompress(packed, MaxStateBytes); err == nil && !bytes.Equal(out, state) {
						err = fmt.Errorf("goroutine %d: round trip changed %d bytes", g, len(state))
					}
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	small := states[6]
	const runs = 200
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		if _, err := Deflate.Compress(small); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if perCall := (after.TotalAlloc - before.TotalAlloc) / runs; perCall > 512<<10 {
		t.Fatalf("compressing %d bytes allocates %d bytes per call on average; the compressor is not reused", len(small), perCall)
	}
}
