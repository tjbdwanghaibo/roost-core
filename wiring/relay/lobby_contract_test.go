package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/service/lobby"
	domain "github.com/tjbdwanghaibo/roost-core/service/relay"
)

// relay 与 lobby 两边各写一份契约（不互相导入业务包）；这里把两份钉在一起：主题、事件名、命令名、JSON 字段。
func TestRelayMatchesTheLobbyContract(t *testing.T) {
	if EventSubject("s") != lobby.EventSubject("s") || CommandSubject("s", "relay-1") != lobby.RelayCommandSubject("s", "relay-1") {
		t.Fatal("NATS subjects differ from the lobby's")
	}
	if commandLeave != lobby.CommandLeave {
		t.Fatal("leave command name differs")
	}
	if VerifySubject("s") != lobby.VerifySubject("s") || verifyUnavailable != lobby.VerifyUnavailable {
		t.Fatal("verification subject or the unavailable reason differs from the lobby's")
	}
	if wire.RelayReasonKicked.String() != lobby.ReasonKicked {
		t.Fatal("the kicked reason in member_left differs from the lobby's")
	}
	// 验权请求与答复的 JSON 字段。
	data, err := json.Marshal(verifyRequest{Room: "r", Account: "a", Relay: "relay-1", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	var asked lobby.RelayVerifyRequest
	if err := json.Unmarshal(data, &asked); err != nil || asked != (lobby.RelayVerifyRequest{Room: "r", Account: "a", Relay: "relay-1", Create: true}) {
		t.Fatalf("lobby reads the verification request as %+v: %v", asked, err)
	}
	for _, answer := range []lobby.RelayVerifyReply{{OK: true}, {Reason: lobby.VerifyKicked}} {
		data, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		var got verifyReply
		if err := json.Unmarshal(data, &got); err != nil || got != (verifyReply{OK: answer.OK, Reason: answer.Reason}) {
			t.Fatalf("relay reads the verification reply %+v as %+v: %v", answer, got, err)
		}
	}
	for relayName, lobbyName := range map[string]string{
		domain.EventHeartbeat: lobby.EventHeartbeat, domain.EventMemberJoined: lobby.EventMemberJoined,
		domain.EventMemberLeft: lobby.EventMemberLeft, domain.EventHostChanged: lobby.EventHostChanged, domain.EventClosed: lobby.EventClosed,
	} {
		if relayName != lobbyName {
			t.Fatalf("event %q differs from the lobby's %q", relayName, lobbyName)
		}
	}

	for _, report := range []domain.Report{
		{Type: domain.EventHostChanged, Room: "r", Relay: "relay-1", Seq: 9, Online: []string{"a", "b"}, Host: "b", Epoch: 3, Account: "b", Slot: 2},
	} {
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		var event lobby.RoomEvent
		if err := json.Unmarshal(data, &event); err != nil || event.Type != lobby.EventHostChanged || event.Host != "b" || event.Epoch != 3 {
			t.Fatalf("lobby reads host_changed as %+v: %v", event, err)
		}
	}

	report := domain.Report{Type: domain.EventMemberLeft, Room: "r", Relay: "relay-1", Seq: 7, Online: []string{"host", "b"}, Host: "host", Epoch: 2,
		Account: "a", Slot: 3, Reason: "kicked"}
	data, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var event lobby.RoomEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	want := lobby.RoomEvent{Type: report.Type, Room: report.Room, Relay: report.Relay, Seq: report.Seq, Online: report.Online, Host: report.Host,
		Epoch: report.Epoch, Account: report.Account, Reason: report.Reason}
	if !reflect.DeepEqual(event, want) {
		t.Fatalf("lobby reads %+v, want %+v", event, want)
	}

	advert := Advertisement{Instance: "relay-1", TCPAddr: "10.0.0.5:7100", WSAddr: "wss://relay.example.com/relay", Rooms: 12}
	data, err = json.Marshal(advert)
	if err != nil {
		t.Fatal(err)
	}
	var registered lobby.Relay
	if err := json.Unmarshal(data, &registered); err != nil {
		t.Fatal(err)
	}
	if registered != (lobby.Relay{Instance: advert.Instance, TCPAddr: advert.TCPAddr, WSAddr: advert.WSAddr, Rooms: advert.Rooms}) {
		t.Fatalf("lobby reads the advertisement as %+v", registered)
	}

	data, err = json.Marshal(lobby.RelayCommand{Type: lobby.CommandLeave, Room: "r", Account: "a"})
	if err != nil {
		t.Fatal(err)
	}
	var cmd command
	if err := json.Unmarshal(data, &cmd); err != nil || cmd != (command{Type: commandLeave, Room: "r", Account: "a"}) {
		t.Fatalf("relay reads the lobby command as %+v: %v", cmd, err)
	}
}

// lobbyBridge 把 relay 与进程内的 lobby.Service 直接接起来：报告走 HandleRelayEventData，验权走 HandleRelayVerifyData，
// 编码都是线上的 JSON（与 NATS 上的一样）。只实现 natsAdmission 用到的 RequestContext。
type lobbyBridge struct {
	fnats.RawClient
	t     *testing.T
	lobby *lobby.Service
	crash *atomic.Bool // 为 true 时报告不再送达（模拟 relay 进程被 kill -9：没有 room.closed）
}

func (b lobbyBridge) RequestContext(ctx context.Context, _ string, data []byte) ([]byte, error) {
	return b.lobby.HandleRelayVerifyData(ctx, data), nil
}

func (b lobbyBridge) Report(report domain.Report) {
	if b.crash.Load() {
		return
	}
	data, err := json.Marshal(report)
	if err == nil {
		err = b.lobby.HandleRelayEventData(context.Background(), data)
	}
	if err != nil {
		b.t.Errorf("lobby did not take %+v: %v", report, err)
	}
}

type bridgeTokens map[string]string // 游戏账号 token → 账号

func (b bridgeTokens) ValidateGameToken(_ context.Context, gameToken, _ string) (string, error) {
	if account, ok := b[gameToken]; ok {
		return account, nil
	}
	return "", fmt.Errorf("%w: unknown test token", lobby.ErrUnauthorized)
}

type bridgeRelays []lobby.Relay

func (b bridgeRelays) Relays(context.Context) ([]lobby.Relay, error) { return b, nil }

type dropCommands struct{}

func (dropCommands) Publish(string, []byte) error { return nil }

// 被踢记在 lobby（方案 §4.4、§6 第 11 条）：房主踢人 → relay 报 member_left{kicked} → lobby 记进被踢名单，join 被拒；
// relay 重启（本地的被踢记录没了）后，被踢的人拿着没过期的旧令牌仍然进不来：重启即丢房，房间不能在新进程上重建
// （房主建房被 lobby 拒绝），成员的旧令牌对应的房间不存在。
func TestKickIsRecordedInTheLobbyAndEnforcedOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	key := []byte(testKey)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	rooms, err := lobby.NewMemoryRooms(time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := lobby.New(lobby.Config{
		Games: map[string]lobby.GameRule{"party": {MaxMembers: 4}}, Rooms: rooms,
		Tokens: bridgeTokens{"gt-host": "host", "gt-alice": "acc-alice"}, Relays: bridgeRelays{{Instance: "relay-3", TCPAddr: addr}},
		Commands: dropCommands{}, Subject: "t.lobby", RelayTokenKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge := lobbyBridge{t: t, lobby: service, crash: &atomic.Bool{}}
	startRelay := func(listener net.Listener) *domain.Server {
		server, err := domain.NewServer(domain.Config{Instance: "relay-3", TokenKey: key}, bridge, natsAdmission{client: bridge, subject: VerifySubject("t.lobby")})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() {
			stop, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_ = server.Shutdown(stop)
		})
		return server
	}
	first := startRelay(listener)

	created, err := service.CreateRoom(ctx, "gt-host", lobby.CreateRequest{Game: "party", Mode: "duel"})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Room.ID
	host := dialWithToken(t, addr, created.Token)
	host.expectControl(t, wire.RelayOpWelcome, nil)
	joined, err := service.JoinRoom(ctx, "gt-alice", id, lobby.JoinRequest{Game: "party"})
	if err != nil {
		t.Fatal(err)
	}
	alice := dialWithToken(t, addr, joined.Token)
	alice.expectControl(t, wire.RelayOpWelcome, nil)
	host.expectControl(t, wire.RelayOpMemberJoined, nil)

	if err := wire.WriteRelay(host.conn, []*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpKick, Seq: 2}}, 0); err != nil {
		t.Fatal(err)
	}
	alice.expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonKicked)})
	deadline := time.Now().Add(3 * time.Second)
	for {
		room, _, err := rooms.Get(ctx, id)
		if err == nil && slices.Contains(room.Kicked, "acc-alice") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lobby never recorded the kick: %+v %v", room, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := service.JoinRoom(ctx, "gt-alice", id, lobby.JoinRequest{Game: "party"}); err == nil || !errors.Is(err, lobby.ErrKicked) {
		t.Fatalf("kicked account joining: %v, want ErrKicked", err)
	}

	// relay 进程没了又起来（kill -9 后重启：lobby 收不到 room.closed，房间记录还在）：房间与本地的被踢记录都没了。
	// 重启即丢房（维护者 2026-11-17 选 A）：房主用 join 重新拿令牌回来也不能在新进程上重建这个房间（lobby 验权答
	// room_not_found，relay 关成 lobby_rejected）；被踢的人拿旧令牌（还没过期）连进来，新进程上没有这个房间（room_not_found）。
	bridge.crash.Store(true)
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := first.Shutdown(stop); err != nil {
		t.Fatal(err)
	}
	bridge.crash.Store(false)
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	startRelay(again)
	rejoined, err := service.JoinRoom(ctx, "gt-host", id, lobby.JoinRequest{Game: "party"})
	if err != nil {
		t.Fatal(err)
	}
	dialWithToken(t, addr, rejoined.Token).expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonLobbyRejected)})
	dialWithToken(t, addr, joined.Token).expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonRoomNotFound)})
}
