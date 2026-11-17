package lobby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

var testKey = []byte("lobby-test-relay-token-key-0123")

// identity 是替身 account 认得的一张游戏账号 token。
type identity struct{ account, game string }

// fakeTokens 代替 account 的 ValidateGameToken：token → (账号, 游戏)，游戏不符按无效处理。
type fakeTokens map[string]identity

func (f fakeTokens) ValidateGameToken(_ context.Context, gameToken, game string) (string, error) {
	id, ok := f[gameToken]
	if !ok || id.game != game {
		return "", fmt.Errorf("%w: test validator refused", ErrUnauthorized)
	}
	return id.account, nil
}

type fakeRelays []Relay

func (f fakeRelays) Relays(context.Context) ([]Relay, error) { return f, nil }

type published struct {
	subject string
	command RelayCommand
}

type recordingPublisher struct {
	mu   sync.Mutex
	sent []published
}

func (p *recordingPublisher) Publish(subject string, data []byte) error {
	var command RelayCommand
	if err := json.Unmarshal(data, &command); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, published{subject, command})
	return nil
}

func (p *recordingPublisher) commands() []published {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]published(nil), p.sent...)
}

type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	svc       *Service
	rooms     RoomStore
	clock     *manualClock
	commands  *recordingPublisher
	redisAddr string
}

// 测试用的 token：host / alice / bob / carol 玩 party，dave 玩 brawl。
var testTokens = fakeTokens{
	"gt-host":  {"acc-host", "party"},
	"gt-alice": {"acc-alice", "party"},
	"gt-bob":   {"acc-bob", "party"},
	"gt-carol": {"acc-carol", "party"},
	"gt-dave":  {"acc-dave", "brawl"},
}

var testRelays = fakeRelays{
	{Instance: "relay-a", TCPAddr: "10.0.0.1:7100", Rooms: 5},
	{Instance: "relay-b", TCPAddr: "10.0.0.2:7100", Rooms: 2},
	{Instance: "relay-c", TCPAddr: "10.0.0.3:7100", WSAddr: "wss://relay-c.example.com/relay", Rooms: 2},
}

// backends 是每条用例要跑的存储：内存总是跑；设了 ROOST_REDIS_TEST_ADDR（~/.roost-it 的 Redis，CI 的 service-redis job）
// 时 Redis 再跑一遍。
func backends(t *testing.T, ttl time.Duration, run func(t *testing.T, h *harness)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		clock := &manualClock{now: time.Now()}
		rooms, err := NewMemoryRooms(ttl, clock.Now)
		if err != nil {
			t.Fatal(err)
		}
		run(t, newHarness(t, rooms, clock, ""))
	})
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
		if addr == "" {
			t.Skip("ROOST_REDIS_TEST_ADDR is not set")
		}
		prefix := redistest.Prefix(t, addr, "roost:test:{lobby}")
		client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		rooms, err := NewRedisRooms(client, prefix, ttl)
		if err != nil {
			t.Fatal(err)
		}
		run(t, newHarness(t, rooms, &manualClock{now: time.Now()}, addr))
	})
}

func newHarness(t *testing.T, rooms RoomStore, clock *manualClock, redisAddr string) *harness {
	t.Helper()
	commands := &recordingPublisher{}
	svc, err := New(Config{
		Games: map[string]GameRule{"party": {MaxMembers: 4}, "brawl": {MaxMembers: 2}},
		Rooms: rooms, Tokens: testTokens, Relays: testRelays, Commands: commands, Subject: "roost.test.lobby",
		RelayTokenKey: testKey, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{svc: svc, rooms: rooms, clock: clock, commands: commands, redisAddr: redisAddr}
}

func (h *harness) create(t *testing.T, gameToken string, req CreateRequest) Ticket {
	t.Helper()
	if req.Game == "" {
		req.Game = "party"
	}
	if req.Mode == "" {
		req.Mode = "duel"
	}
	ticket, err := h.svc.CreateRoom(context.Background(), gameToken, req)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	return ticket
}

// confirm 模拟 relay 报告房主已连上（第一条心跳），房间从此出现在列表里。
func (h *harness) report(t *testing.T, event RoomEvent) {
	t.Helper()
	if err := h.svc.HandleRelayEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleRelayEvent(%+v): %v", event, err)
	}
}

func (h *harness) join(gameToken, roomID string, req JoinRequest) (Ticket, error) {
	if req.Game == "" {
		req.Game = "party"
	}
	return h.svc.JoinRoom(context.Background(), gameToken, roomID, req)
}

func (h *harness) room(t *testing.T, id string) Room {
	t.Helper()
	room, found, err := h.rooms.Get(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Get(%s) = found %v, %v", id, found, err)
	}
	return room
}

func verifyToken(t *testing.T, ticket Ticket, relay string) token.Claims {
	t.Helper()
	claims, err := token.Verify(testKey, ticket.Token, relay, time.Now())
	if err != nil {
		t.Fatalf("relay token does not verify for %s: %v", relay, err)
	}
	if claims.Room != ticket.Room.ID || claims.ExpiresAt.Unix() != ticket.ExpiresAt {
		t.Fatalf("claims %+v do not match ticket %+v", claims, ticket)
	}
	return claims
}

// 建房：选房间数最少、且有所要接入方式的 relay（同数取实例名小的），返回该接入方式的地址，签房主令牌；
// 人数缺省取游戏规则、不能超过它。
func TestCreateRoomPicksLeastLoadedRelayAndSignsHostToken(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		tcp := h.create(t, "gt-host", CreateRequest{Name: "first"})
		if tcp.RelayAddr != "10.0.0.2:7100" {
			t.Fatalf("tcp relay addr = %q, want relay-b's", tcp.RelayAddr)
		}
		claims := verifyToken(t, tcp, "relay-b")
		if claims.Role != token.RoleHost || claims.Account != "acc-host" || claims.MaxMembers != 4 {
			t.Fatalf("claims = %+v, want host acc-host with the game's 4 seats", claims)
		}
		if tcp.Room.Host != "acc-host" || tcp.Room.Members != 1 || tcp.Room.State != StateOpen || tcp.Room.HostEpoch != 0 {
			t.Fatalf("room view = %+v", tcp.Room)
		}

		ws := h.create(t, "gt-host", CreateRequest{Transport: TransportWS, MaxMembers: 3})
		if ws.RelayAddr != "wss://relay-c.example.com/relay" {
			t.Fatalf("ws relay addr = %q, want relay-c's wss url", ws.RelayAddr)
		}
		if claims := verifyToken(t, ws, "relay-c"); claims.MaxMembers != 3 {
			t.Fatalf("max members in token = %d, want 3", claims.MaxMembers)
		}

		_, err := h.svc.CreateRoom(context.Background(), "gt-host", CreateRequest{Game: "party", Mode: "duel", MaxMembers: 5})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("max_members above the game rule: err = %v, want ErrBadRequest", err)
		}
		_, err = h.svc.CreateRoom(context.Background(), "gt-host", CreateRequest{Game: "party", Mode: "duel", Attrs: json.RawMessage(`"` + string(make([]byte, MaxAttrsBytes)) + `"`)})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("oversized attrs: err = %v, want ErrBadRequest", err)
		}
	})
}

func TestCreateRoomWithoutAnyRelayForTheTransport(t *testing.T) {
	clock := &manualClock{now: time.Now()}
	rooms, _ := NewMemoryRooms(time.Hour, clock.Now)
	svc, err := New(Config{
		Games: map[string]GameRule{"party": {MaxMembers: 4}}, Rooms: rooms, Tokens: testTokens,
		Relays: fakeRelays{{Instance: "relay-a", TCPAddr: "10.0.0.1:7100"}}, Commands: &recordingPublisher{},
		Subject: "s", RelayTokenKey: testKey, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateRoom(context.Background(), "gt-host", CreateRequest{Game: "party", Mode: "duel", Transport: TransportWS})
	if !errors.Is(err, ErrNoRelay) {
		t.Fatalf("err = %v, want ErrNoRelay", err)
	}
}

// 鉴权：没有 token、拿别的游戏的 token、请求的游戏不在 lobby.games 里，都拒绝；房间按 game 隔开，拿 brawl 的身份
// 查不到 party 的房间。
func TestRequestsNeedAGameTokenForTheRequestedGame(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		if _, err := h.svc.CreateRoom(ctx, "", CreateRequest{Game: "party", Mode: "duel"}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("no token: err = %v, want ErrUnauthorized", err)
		}
		if _, err := h.svc.CreateRoom(ctx, "gt-dave", CreateRequest{Game: "party", Mode: "duel"}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("brawl token in the party lobby: err = %v, want ErrUnauthorized", err)
		}
		if _, err := h.svc.CreateRoom(ctx, "gt-host", CreateRequest{Game: "chess", Mode: "duel"}); !errors.Is(err, ErrGameNotServed) {
			t.Fatalf("game not served: err = %v, want ErrGameNotServed", err)
		}
		room := h.create(t, "gt-host", CreateRequest{})
		if _, err := h.join("gt-dave", room.Room.ID, JoinRequest{Game: "brawl"}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("brawl player joining a party room: err = %v, want ErrRoomMissing", err)
		}
		if _, err := h.svc.ListRooms(ctx, "gt-alice", "brawl", "duel", 0); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("party token listing brawl: err = %v, want ErrUnauthorized", err)
		}
	})
}

// 满员：席位含房主；满了再来的人被拒，已在房间里的人再 join 是幂等的（不占新席位、重新签令牌）；
// 预留过期又没连上 relay 的人让出席位。
func TestJoinRefusesWhenFullAndIsIdempotentForMembers(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		created := h.create(t, "gt-host", CreateRequest{MaxMembers: 2})
		id := created.Room.ID
		alice, err := h.join("gt-alice", id, JoinRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if claims := verifyToken(t, alice, "relay-b"); claims.Role != token.RoleMember || claims.Account != "acc-alice" {
			t.Fatalf("alice's claims = %+v", claims)
		}
		if _, err := h.join("gt-bob", id, JoinRequest{}); !errors.Is(err, ErrRoomFull) {
			t.Fatalf("bob into a full room: err = %v, want ErrRoomFull", err)
		}
		again, err := h.join("gt-alice", id, JoinRequest{})
		if err != nil {
			t.Fatalf("alice rejoining: %v", err)
		}
		if again.Room.Members != 2 || again.Token == "" {
			t.Fatalf("rejoin = %+v, want the same two seats and a fresh token", again.Room)
		}
		host, err := h.join("gt-host", id, JoinRequest{})
		if err != nil {
			t.Fatalf("host rejoining a full room: %v", err)
		}
		if claims := verifyToken(t, host, "relay-b"); claims.Role != token.RoleHost {
			t.Fatalf("host rejoin role = %s", claims.Role)
		}

		// alice 拿了令牌却一直没连上：预留过期后席位让出。
		h.clock.Advance(token.DefaultTTL + reservationSlack + time.Second)
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatalf("bob after alice's reservation lapsed: %v", err)
		}
	})
}

// 密码：错的拒绝，对的放行；已在房间里的人重连不用再给密码。
func TestJoinChecksThePassword(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		created := h.create(t, "gt-host", CreateRequest{Password: "s3cret"})
		if !created.Room.HasPassword {
			t.Fatal("room view does not say it has a password")
		}
		stored := h.room(t, created.Room.ID)
		if stored.PasswordHash == "" || stored.PasswordHash == "s3cret" {
			t.Fatalf("password stored as %q, want a hash", stored.PasswordHash)
		}
		for _, wrong := range []string{"", "S3CRET", "s3cret "} {
			if _, err := h.join("gt-alice", created.Room.ID, JoinRequest{Password: wrong}); !errors.Is(err, ErrWrongPassword) {
				t.Fatalf("password %q: err = %v, want ErrWrongPassword", wrong, err)
			}
		}
		if _, err := h.join("gt-alice", created.Room.ID, JoinRequest{Password: "s3cret"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.join("gt-alice", created.Room.ID, JoinRequest{}); err != nil {
			t.Fatalf("member reconnecting without the password: %v", err)
		}
	})
}

// 房主改状态：只有房主能改；改成已开始后不出现在列表里、新人进不来、老成员照样能重连；改回可加入又能看到。
func TestOnlyTheHostChangesStateAndStartedRoomsAreClosedToNewcomers(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		created := h.create(t, "gt-host", CreateRequest{})
		id := created.Room.ID
		if _, err := h.join("gt-alice", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: []string{"acc-host", "acc-alice"}})

		if _, err := h.svc.SetRoomState(ctx, "gt-alice", id, StateRequest{Game: "party", State: StateStarted}); !errors.Is(err, ErrNotHost) {
			t.Fatalf("member changing state: err = %v, want ErrNotHost", err)
		}
		started, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted, Attrs: json.RawMessage(`{"map":"desert"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if started.Room.State != StateStarted || string(started.Room.Attrs) != `{"map":"desert"}` || started.Token != "" {
			t.Fatalf("state reply = %+v", started)
		}
		if listed := h.list(t, "duel"); len(listed) != 0 {
			t.Fatalf("started room is listed: %+v", listed)
		}
		if _, err := h.join("gt-bob", id, JoinRequest{}); !errors.Is(err, ErrRoomStarted) {
			t.Fatalf("newcomer into a started room: err = %v, want ErrRoomStarted", err)
		}
		if _, err := h.join("gt-alice", id, JoinRequest{}); err != nil {
			t.Fatalf("member reconnecting to a started room: %v", err)
		}
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateOpen}); err != nil {
			t.Fatal(err)
		}
		if listed := h.list(t, "duel"); len(listed) != 1 || listed[0].ID != id {
			t.Fatalf("reopened room not listed: %+v", listed)
		}
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: "paused"}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("unknown state: err = %v, want ErrBadRequest", err)
		}
	})
}

func (h *harness) list(t *testing.T, mode string) []RoomView {
	t.Helper()
	rooms, err := h.svc.ListRooms(context.Background(), "gt-carol", "party", mode, 0)
	if err != nil {
		t.Fatalf("ListRooms: %v", err)
	}
	return rooms
}

// 列房过滤：只列同 game/mode、relay 已报告房主连上、可加入、未满、未关闭的房间，新建的在前。
func TestListShowsOnlyJoinableConfirmedRoomsOfTheMode(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		older := h.create(t, "gt-host", CreateRequest{})
		h.clock.Advance(2 * time.Second)
		newer := h.create(t, "gt-alice", CreateRequest{})
		otherMode := h.create(t, "gt-bob", CreateRequest{Mode: "ffa"})
		full := h.create(t, "gt-carol", CreateRequest{MaxMembers: 1})
		unconfirmed := h.create(t, "gt-host", CreateRequest{})
		closed := h.create(t, "gt-bob", CreateRequest{})
		for _, ticket := range []Ticket{older, newer, otherMode, full, closed} {
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: ticket.Room.ID, Relay: "relay-b", Seq: 1, Host: ticket.Room.Host, Online: []string{ticket.Room.Host}})
		}
		h.report(t, RoomEvent{Type: EventClosed, Room: closed.Room.ID, Relay: "relay-b", Seq: 2})

		listed := h.list(t, "duel")
		if len(listed) != 2 || listed[0].ID != newer.Room.ID || listed[1].ID != older.Room.ID {
			t.Fatalf("listed = %+v, want [newer, older] only (unconfirmed %s, full %s, closed %s excluded)",
				listed, unconfirmed.Room.ID, full.Room.ID, closed.Room.ID)
		}
		if ffa := h.list(t, "ffa"); len(ffa) != 1 || ffa[0].ID != otherMode.Room.ID {
			t.Fatalf("ffa listed = %+v", ffa)
		}
		if _, err := h.svc.ListRooms(context.Background(), "gt-carol", "party", "bad mode!", 0); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("bad mode: err = %v, want ErrBadRequest", err)
		}
		if _, err := h.join("gt-alice", closed.Room.ID, JoinRequest{}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("joining a closed room: err = %v, want ErrRoomMissing", err)
		}
	})
}

// relay 的 host_changed 乱序到达：只接受更大的 epoch，迟到的、相等的都忽略；心跳带着的 epoch 同样按 CAS 处理。
func TestHostChangedAcceptsOnlyLargerEpochs(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		created := h.create(t, "gt-host", CreateRequest{})
		id := created.Room.ID
		online := []string{"acc-host", "acc-alice", "acc-bob"}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: online})

		h.report(t, RoomEvent{Type: EventHostChanged, Room: id, Relay: "relay-b", Seq: 5, Host: "acc-bob", Epoch: 2, Online: online})
		h.report(t, RoomEvent{Type: EventHostChanged, Room: id, Relay: "relay-b", Seq: 4, Host: "acc-alice", Epoch: 1, Online: online})
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 6, Host: "acc-alice", Epoch: 2, Online: online})
		if room := h.room(t, id); room.Host != "acc-bob" || room.HostEpoch != 2 {
			t.Fatalf("after epochs 2 then 1 then 2 again: host %s epoch %d, want acc-bob at 2", room.Host, room.HostEpoch)
		}
		// 另一个 relay 发来的报告整条忽略。
		h.report(t, RoomEvent{Type: EventHostChanged, Room: id, Relay: "relay-a", Seq: 9, Host: "acc-alice", Epoch: 7, Online: online})
		h.report(t, RoomEvent{Type: EventHostChanged, Room: id, Relay: "relay-b", Seq: 7, Host: "acc-alice", Epoch: 3, Online: online})
		if room := h.room(t, id); room.Host != "acc-alice" || room.HostEpoch != 3 {
			t.Fatalf("after epoch 3: host %s epoch %d, want acc-alice at 3", room.Host, room.HostEpoch)
		}
	})
}

// 换 host 之后签的令牌按新房主填角色：新房主拿 host、旧房主拿 member；/state 只认新房主。
func TestTokenRoleFollowsTheHostAfterAHostChange(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		created := h.create(t, "gt-host", CreateRequest{})
		id := created.Room.ID
		if _, err := h.join("gt-alice", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHostChanged, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-alice", Epoch: 1, Online: []string{"acc-alice"}})

		alice, err := h.join("gt-alice", id, JoinRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if claims := verifyToken(t, alice, "relay-b"); claims.Role != token.RoleHost {
			t.Fatalf("new host's token role = %s, want host", claims.Role)
		}
		old, err := h.join("gt-host", id, JoinRequest{})
		if err != nil {
			t.Fatalf("old host rejoining: %v", err)
		}
		if claims := verifyToken(t, old, "relay-b"); claims.Role != token.RoleMember {
			t.Fatalf("old host's token role = %s, want member", claims.Role)
		}
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); !errors.Is(err, ErrNotHost) {
			t.Fatalf("old host changing state: err = %v, want ErrNotHost", err)
		}
		if _, err := h.svc.SetRoomState(ctx, "gt-alice", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
			t.Fatalf("new host changing state: %v", err)
		}
	})
}

// relay 报告维护席位：在线名单只认更大的序号；离线的成员让出席位（房主除外）；relay 报告在线而目录没有的人补上；
// 关闭的房间不再接受报告。
func TestRelayReportsMaintainSeats(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		created := h.create(t, "gt-host", CreateRequest{})
		id := created.Room.ID
		for _, tok := range []string{"gt-alice", "gt-bob"} {
			if _, err := h.join(tok, id, JoinRequest{}); err != nil {
				t.Fatal(err)
			}
		}
		h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: "relay-b", Seq: 2, Host: "acc-host", Account: "acc-bob",
			Online: []string{"acc-host", "acc-alice", "acc-bob"}})
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 3, Host: "acc-host", Account: "acc-alice",
			Online: []string{"acc-bob"}})
		// 迟到的旧报告不改名单。
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 2, Host: "acc-host",
			Online: []string{"acc-host", "acc-alice", "acc-bob"}})
		room := h.room(t, id)
		if got := seats(room); got != "acc-host:offline acc-bob:online" {
			t.Fatalf("seats = %q, want the host kept offline and bob online", got)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 4, Host: "acc-host",
			Online: []string{"acc-host", "acc-bob", "acc-carol"}})
		if got := seats(h.room(t, id)); got != "acc-host:online acc-bob:online acc-carol:online" {
			t.Fatalf("seats = %q, want carol added from the relay's list", got)
		}
		if err := h.svc.HandleRelayEvent(context.Background(), RoomEvent{Type: "room.bogus", Room: id, Relay: "relay-b"}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("unknown event type: err = %v, want ErrBadRequest", err)
		}
		if err := h.svc.HandleRelayEvent(context.Background(), RoomEvent{Type: EventHeartbeat, Room: "no-such-room", Relay: "relay-b", Seq: 1}); err != nil {
			t.Fatalf("report for an unknown room: %v, want it ignored", err)
		}
		h.report(t, RoomEvent{Type: EventClosed, Room: id, Relay: "relay-b", Seq: 5})
		h.report(t, RoomEvent{Type: EventHostChanged, Room: id, Relay: "relay-b", Seq: 6, Host: "acc-bob", Epoch: 1, Online: []string{"acc-bob"}})
		if room := h.room(t, id); !room.Closed || room.Host != "acc-host" {
			t.Fatalf("closed room accepted a later report: %+v", room)
		}
	})
}

// 被踢记在 lobby（方案 §4.4）：relay 报 member_left、原因 kicked，lobby 把账号记进被踢名单、让出席位，之后
// join 被拒（ErrKicked）；迟到的报告也照记；relay 的在线名单里再出现它也不补回来。名单随房间记录一起过期。
func TestKickedAccountsAreRecordedAndRefused(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		created := h.create(t, "gt-host", CreateRequest{})
		id := created.Room.ID
		for _, tok := range []string{"gt-alice", "gt-bob"} {
			if _, err := h.join(tok, id, JoinRequest{}); err != nil {
				t.Fatal(err)
			}
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 5, Host: "acc-host", Online: []string{"acc-host", "acc-alice", "acc-bob"}})
		// 序号比已应用的小（迟到）也照记：踢出是永久的事实。
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 4, Host: "acc-host", Account: "acc-alice", Reason: ReasonKicked,
			Online: []string{"acc-host", "acc-bob"}})
		room := h.room(t, id)
		if !slices.Equal(room.Kicked, []string{"acc-alice"}) {
			t.Fatalf("after the kick: kicked %v seats %q", room.Kicked, seats(room))
		}
		if room.memberIndex("acc-alice") >= 0 {
			t.Fatalf("kicked account still holds a seat: %q", seats(room))
		}
		if _, err := h.join("gt-alice", id, JoinRequest{}); !errors.Is(err, ErrKicked) {
			t.Fatalf("kicked account joining: err = %v, want ErrKicked", err)
		}
		// 普通的离开不是被踢。
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 6, Host: "acc-host", Account: "acc-bob", Reason: "disconnected",
			Online: []string{"acc-host"}})
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatalf("disconnected member rejoining: %v", err)
		}
		// relay 报告在线也不把被踢的人补回席位。
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 7, Host: "acc-host", Online: []string{"acc-host", "acc-alice"}})
		if room := h.room(t, id); room.memberIndex("acc-alice") >= 0 {
			t.Fatalf("kicked account re-added from the relay's list: %q", seats(room))
		}
	})
}

// relay 每条连接的验权（方案 §4.3）：房间还在、分在这个 relay、没被踢、占着席位；建房的连接还要是目录里的房主。
// 不核对角色：换 host 之后的新旧房主都按“能不能进房”答。
func TestRelayVerification(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		created := h.create(t, "gt-host", CreateRequest{})
		id := created.Room.ID
		if _, err := h.join("gt-alice", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		verify := func(account, relay string, create bool) RelayVerifyReply {
			return h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: id, Account: account, Relay: relay, Create: create})
		}
		for name, c := range map[string]struct {
			got  RelayVerifyReply
			want RelayVerifyReply
		}{
			"host creates":        {verify("acc-host", "relay-b", true), RelayVerifyReply{OK: true}},
			"member":              {verify("acc-alice", "relay-b", false), RelayVerifyReply{OK: true}},
			"member creates":      {verify("acc-alice", "relay-b", true), RelayVerifyReply{Reason: VerifyNotHost}},
			"stranger":            {verify("acc-bob", "relay-b", false), RelayVerifyReply{Reason: VerifyNotMember}},
			"other relay":         {verify("acc-alice", "relay-a", false), RelayVerifyReply{Reason: VerifyWrongRelay}},
			"missing room":        {h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: "nope", Account: "acc-alice", Relay: "relay-b"}), RelayVerifyReply{Reason: VerifyRoomNotFound}},
			"missing fields":      {h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: id}), RelayVerifyReply{Reason: VerifyBadRequest}},
			"undecodable request": {decodeReply(t, h.svc.HandleRelayVerifyData(ctx, []byte("{"))), RelayVerifyReply{Reason: VerifyBadRequest}},
			"decoded member":      {decodeReply(t, h.svc.HandleRelayVerifyData(ctx, []byte(`{"room":"`+id+`","account":"acc-alice","relay":"relay-b"}`))), RelayVerifyReply{OK: true}},
		} {
			if c.got != c.want {
				t.Fatalf("%s: %+v, want %+v", name, c.got, c.want)
			}
		}

		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 2, Host: "acc-host", Account: "acc-alice", Reason: ReasonKicked,
			Online: []string{"acc-host"}})
		if got := verify("acc-alice", "relay-b", false); got != (RelayVerifyReply{Reason: VerifyKicked}) {
			t.Fatalf("kicked member: %+v", got)
		}
		// 主动转交之后旧房主仍连着、占着席位：重连能进（角色由 relay 定，只能当成员）；新房主同样能进。
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHostChanged, Room: id, Relay: "relay-b", Seq: 3, Host: "acc-bob", Epoch: 1, Online: []string{"acc-host", "acc-bob"}})
		if got := verify("acc-host", "relay-b", false); !got.OK {
			t.Fatalf("old host still seated: %+v", got)
		}
		if got := verify("acc-bob", "relay-b", false); !got.OK {
			t.Fatalf("new host: %+v", got)
		}
		if got := verify("acc-host", "relay-b", true); got != (RelayVerifyReply{Reason: VerifyNotHost}) {
			t.Fatalf("old host creating: %+v", got)
		}
		h.report(t, RoomEvent{Type: EventClosed, Room: id, Relay: "relay-b", Seq: 4})
		if got := verify("acc-bob", "relay-b", false); got != (RelayVerifyReply{Reason: VerifyRoomNotFound}) {
			t.Fatalf("closed room: %+v", got)
		}
	})
}

// relay 重启即丢房（维护者 2026-11-17 选 A，多进程演练 3b）：房间被 relay 报告过之后，任何连接都不能再用它建房。
// relay 在 room_ttl 内重启时，新进程上没有这个房间，房主拿着没过期的令牌重连会请求建房：以前验权放行，同一房间号在
// 新进程上重建，比房主早到的成员收到 room_not_found、报告 seq 从 1 重数。现在答 room_not_found，房间按“relay 崩溃时
// 房间丢失”处理，目录在 room_ttl 内消失。还没被报告过的房间（房主还没连上）照常建房。
func TestARoomTheRelayHasReportedCannotBeCreatedAgain(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		id := h.create(t, "gt-host", CreateRequest{}).Room.ID
		relay := h.room(t, id).Relay.Instance
		verify := func(create bool) RelayVerifyReply {
			return h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: id, Account: "acc-host", Relay: relay, Create: create})
		}
		if got := verify(true); !got.OK {
			t.Fatalf("host creating a room no relay has reported: %+v", got)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})
		if got := verify(true); got != (RelayVerifyReply{Reason: VerifyRoomNotFound}) {
			t.Fatalf("host re-creating a reported room (relay restarted): %+v, want room_not_found", got)
		}
		if got := verify(false); !got.OK {
			t.Fatalf("host reconnecting to the live room: %+v", got)
		}
	})
}

func decodeReply(t *testing.T, data []byte) RelayVerifyReply {
	t.Helper()
	var reply RelayVerifyReply
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func seats(room Room) string {
	out := ""
	for i, member := range room.Members {
		if i > 0 {
			out += " "
		}
		state := "offline"
		if member.Online {
			state = "online"
		}
		out += member.Account + ":" + state
	}
	return out
}

// 离开：成员立即让出席位并让 relay 断开他；房主离开交给 relay 换 host（目录里的房主不变）；relay 还没报告过的房间
// 房主一走就关。不在房间里的人离开是空操作。
func TestLeave(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		created := h.create(t, "gt-host", CreateRequest{MaxMembers: 2})
		id := created.Room.ID
		if _, err := h.join("gt-alice", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: []string{"acc-host", "acc-alice"}})
		left, err := h.svc.LeaveRoom(ctx, "gt-alice", id, LeaveRequest{Game: "party"})
		if err != nil {
			t.Fatal(err)
		}
		if left.Room.Members != 1 || left.Token != "" {
			t.Fatalf("leave reply = %+v", left)
		}
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatalf("bob into the seat alice left: %v", err)
		}
		if _, err := h.svc.LeaveRoom(ctx, "gt-carol", id, LeaveRequest{Game: "party"}); err != nil {
			t.Fatalf("leave by a non-member: %v", err)
		}
		if _, err := h.svc.LeaveRoom(ctx, "gt-host", id, LeaveRequest{Game: "party"}); err != nil {
			t.Fatal(err)
		}
		if room := h.room(t, id); room.Host != "acc-host" || room.Closed {
			t.Fatalf("host leave changed the directory before the relay reported: %+v", room)
		}
		want := []published{
			{"roost.test.lobby.relay.relay-b", RelayCommand{Type: CommandLeave, Room: id, Account: "acc-alice"}},
			{"roost.test.lobby.relay.relay-b", RelayCommand{Type: CommandLeave, Room: id, Account: "acc-host"}},
		}
		if got := h.commands.commands(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("relay commands = %+v, want %+v", got, want)
		}

		early := h.create(t, "gt-carol", CreateRequest{})
		if _, err := h.svc.LeaveRoom(ctx, "gt-carol", early.Room.ID, LeaveRequest{Game: "party"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.join("gt-alice", early.Room.ID, JoinRequest{}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("room whose host left before connecting: err = %v, want ErrRoomMissing", err)
		}
	})
}

// lapse 让所有预留过期：令牌寿命加余量再多一秒。
func (h *harness) lapse() { h.clock.Advance(token.DefaultTTL + reservationSlack + time.Second) }

// 开局后掉线的席位（方案 §6 第 12 条）：已开始的房间里，连上过 relay 的成员掉线后席位一直留着，预留过期也不让出；
// 他再 join 回到原座——不受“已开始”限制、满员也不算新人——签新令牌，relay 验权能过。新人照旧 room_started。
func TestStartedRoomsKeepDroppedMembersSeats(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		created := h.create(t, "gt-host", CreateRequest{MaxMembers: 2})
		id := created.Room.ID
		if _, err := h.join("gt-alice", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: []string{"acc-host", "acc-alice"}})
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
			t.Fatal(err)
		}
		// alice 掉线：relay 的在线名单里没有她，过了预留期，再来一条心跳。
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 2, Host: "acc-host", Account: "acc-alice", Reason: "disconnected",
			Online: []string{"acc-host"}})
		h.lapse()
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 3, Host: "acc-host", Online: []string{"acc-host"}})
		if got := seats(h.room(t, id)); got != "acc-host:online acc-alice:offline" {
			t.Fatalf("seats after alice dropped in a started room = %q, want her seat kept", got)
		}
		if got := h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: id, Account: "acc-alice", Relay: "relay-b"}); !got.OK {
			t.Fatalf("verifying the dropped member: %+v", got)
		}
		// 新人：房间已开始，拒绝（满员之前先答 room_started）。
		_, err := h.join("gt-bob", id, JoinRequest{})
		if _, name := errorStatus(err); !errors.Is(err, ErrRoomStarted) || name != "room_started" {
			t.Fatalf("newcomer into a started room: err = %v (%s), want room_started", err, name)
		}
		// alice 回座：房间已开始且 2/2 满员，仍然放行，人数不变，签新令牌。
		back, err := h.join("gt-alice", id, JoinRequest{})
		if err != nil {
			t.Fatalf("dropped member returning to a started, full room: %v", err)
		}
		if claims := verifyToken(t, back, "relay-b"); claims.Account != "acc-alice" || claims.Role != token.RoleMember || back.Room.Members != 2 {
			t.Fatalf("return ticket: claims %+v, room %+v", claims, back.Room)
		}
		// 回座后令牌又过期、她仍没连上：席位照样留着，验权照样能过。
		h.lapse()
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 4, Host: "acc-host", Online: []string{"acc-host"}})
		if got := h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: id, Account: "acc-alice", Relay: "relay-b"}); !got.OK {
			t.Fatalf("verifying the returned member after her reservation lapsed: %+v", got)
		}
		h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: "relay-b", Seq: 5, Host: "acc-host", Account: "acc-alice",
			Online: []string{"acc-host", "acc-alice"}})
		if got := seats(h.room(t, id)); got != "acc-host:online acc-alice:online" {
			t.Fatalf("seats after alice reconnected = %q", got)
		}
	})
}

// 没开局的房间沿用原规则：掉线并且过了预留期，席位让出，别人能进。
func TestOpenRoomsReleaseDroppedMembersSeats(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		created := h.create(t, "gt-host", CreateRequest{MaxMembers: 2})
		id := created.Room.ID
		if _, err := h.join("gt-alice", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: []string{"acc-host", "acc-alice"}})
		h.lapse()
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 2, Host: "acc-host", Account: "acc-alice", Reason: "disconnected",
			Online: []string{"acc-host"}})
		if got := seats(h.room(t, id)); got != "acc-host:online" {
			t.Fatalf("seats after alice dropped in an open room = %q, want hers released", got)
		}
		if got := h.svc.VerifyRelayConnection(context.Background(), RelayVerifyRequest{Room: id, Account: "acc-alice", Relay: "relay-b"}); got.Reason != VerifyNotMember {
			t.Fatalf("verifying the dropped member of an open room: %+v, want not_member", got)
		}
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatalf("bob into the released seat: %v", err)
		}

		// 改回 open 的房间同样按原规则：开局时留着的掉线席位，改回后让出。
		ctx := context.Background()
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: "relay-b", Seq: 3, Host: "acc-host", Account: "acc-bob", Online: []string{"acc-host", "acc-bob"}})
		h.lapse()
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 4, Host: "acc-host", Online: []string{"acc-host"}})
		if got := seats(h.room(t, id)); got != "acc-host:online acc-bob:offline" {
			t.Fatalf("seats in the started room = %q", got)
		}
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateOpen}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 5, Host: "acc-host", Online: []string{"acc-host"}})
		if got := seats(h.room(t, id)); got != "acc-host:online" {
			t.Fatalf("seats after reopening = %q, want the dropped seat released", got)
		}
	})
}

// 开局那一刻还在预留期、从没连上 relay 的人：不算掉线成员，预留过期就让出，之后再 join 是新人（room_started）。
func TestStartingARoomDoesNotKeepSeatsNeverConnected(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		created := h.create(t, "gt-host", CreateRequest{})
		id := created.Room.ID
		if _, err := h.join("gt-alice", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
			t.Fatal(err)
		}
		if got := h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: id, Account: "acc-alice", Relay: "relay-b"}); !got.OK {
			t.Fatalf("alice within her reservation: %+v", got)
		}
		h.lapse()
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 2, Host: "acc-host", Online: []string{"acc-host"}})
		if got := seats(h.room(t, id)); got != "acc-host:online" {
			t.Fatalf("seats = %q, want the never-connected reservation released", got)
		}
		if _, err := h.join("gt-alice", id, JoinRequest{}); !errors.Is(err, ErrRoomStarted) {
			t.Fatalf("alice after her reservation lapsed: err = %v, want ErrRoomStarted", err)
		}
	})
}

// 开局后掉线的人，席位由三种事让出：被房主踢（记进被踢名单，join 答 kicked）、主动离开（lobby 的 leave，或 relay
// 报告 member_left 原因 left）、关房。房主主动离开后换了 host，他也不再占座。
func TestStartedRoomsReleaseSeatsOnKickLeaveAndClose(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		created := h.create(t, "gt-host", CreateRequest{})
		id := created.Room.ID
		for _, tok := range []string{"gt-alice", "gt-bob", "gt-carol"} {
			if _, err := h.join(tok, id, JoinRequest{}); err != nil {
				t.Fatal(err)
			}
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host",
			Online: []string{"acc-host", "acc-alice", "acc-bob", "acc-carol"}})
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
			t.Fatal(err)
		}
		h.lapse()
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 2, Host: "acc-host", Online: []string{"acc-host"}})
		if got := seats(h.room(t, id)); got != "acc-host:online acc-alice:offline acc-bob:offline acc-carol:offline" {
			t.Fatalf("seats after everyone but the host dropped = %q", got)
		}

		// 踢：relay 报告掉线的 alice 被踢（房主踢的是她的离线座位）。
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 3, Host: "acc-host", Account: "acc-alice", Reason: ReasonKicked,
			Online: []string{"acc-host"}})
		_, err := h.join("gt-alice", id, JoinRequest{})
		if _, name := errorStatus(err); !errors.Is(err, ErrKicked) || name != "kicked" {
			t.Fatalf("kicked member returning: err = %v (%s), want kicked", err, name)
		}
		if got := seats(h.room(t, id)); got != "acc-host:online acc-bob:offline acc-carol:offline" {
			t.Fatalf("seats after the kick = %q, want alice's released", got)
		}

		// 主动离开（lobby）：bob 掉线后调 leave，席位让出，之后再 join 是新人。
		if _, err := h.svc.LeaveRoom(ctx, "gt-bob", id, LeaveRequest{Game: "party"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.join("gt-bob", id, JoinRequest{}); !errors.Is(err, ErrRoomStarted) {
			t.Fatalf("bob after leaving: err = %v, want ErrRoomStarted", err)
		}

		// 主动离开（relay）：carol 回座连上，又掉线，再回座连上后发 RelayOpLeave，relay 报告 member_left 原因 left。
		// 先到一条迟到的 left（序号比已应用的小）：不能清掉回座后又连上的她，之后掉线席位照样留着。
		if _, err := h.join("gt-carol", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: "relay-b", Seq: 5, Host: "acc-host", Account: "acc-carol", Online: []string{"acc-host", "acc-carol"}})
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 4, Host: "acc-host", Account: "acc-carol", Reason: ReasonLeft,
			Online: []string{"acc-host"}})
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 6, Host: "acc-host", Account: "acc-carol", Reason: "idle_timeout",
			Online: []string{"acc-host"}})
		if got := seats(h.room(t, id)); got != "acc-host:online acc-carol:offline" {
			t.Fatalf("seats after a late left and a drop = %q, want carol's kept", got)
		}
		h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: "relay-b", Seq: 7, Host: "acc-host", Account: "acc-carol", Online: []string{"acc-host", "acc-carol"}})
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: "relay-b", Seq: 8, Host: "acc-host", Account: "acc-carol", Reason: ReasonLeft,
			Online: []string{"acc-host"}})
		if got := seats(h.room(t, id)); got != "acc-host:online" {
			t.Fatalf("seats after carol left through the relay = %q", got)
		}

		// 房主主动离开：席位等 relay 换完 host 才让出（新房间演示）。
		second := h.create(t, "gt-host", CreateRequest{})
		sid := second.Room.ID
		if _, err := h.join("gt-alice", sid, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: sid, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: []string{"acc-host", "acc-alice"}})
		if _, err := h.svc.SetRoomState(ctx, "gt-host", sid, StateRequest{Game: "party", State: StateStarted}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.LeaveRoom(ctx, "gt-host", sid, LeaveRequest{Game: "party"}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHostChanged, Room: sid, Relay: "relay-b", Seq: 2, Host: "acc-alice", Epoch: 1, Online: []string{"acc-alice"}})
		if got := seats(h.room(t, sid)); got != "acc-alice:online" {
			t.Fatalf("seats after the host left a started room = %q, want the old host's released", got)
		}
		if _, err := h.join("gt-host", sid, JoinRequest{}); !errors.Is(err, ErrRoomStarted) {
			t.Fatalf("old host after leaving: err = %v, want ErrRoomStarted", err)
		}

		// 关房：房间不在了，掉线的人也回不去。
		h.report(t, RoomEvent{Type: EventClosed, Room: sid, Relay: "relay-b", Seq: 3})
		if _, err := h.join("gt-alice", sid, JoinRequest{}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("returning to a closed room: err = %v, want ErrRoomMissing", err)
		}
		if got := h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: sid, Account: "acc-alice", Relay: "relay-b"}); got.Reason != VerifyRoomNotFound {
			t.Fatalf("verifying in a closed room: %+v", got)
		}
	})
}

// Redis 过期：记录的寿命是 room TTL，relay 的每条报告续一次；不再报告的房间到期消失，列表与加入都看不到，
// 索引里残留的房间号在列表时清掉。
func TestRoomsExpireWithoutRelayReports(t *testing.T) {
	const ttl = 1500 * time.Millisecond
	backends(t, ttl, func(t *testing.T, h *harness) {
		wait := func(d time.Duration) {
			if h.redisAddr != "" {
				time.Sleep(d)
			}
			h.clock.Advance(d)
		}
		kept := h.create(t, "gt-host", CreateRequest{})
		dropped := h.create(t, "gt-alice", CreateRequest{})
		for _, ticket := range []Ticket{kept, dropped} {
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: ticket.Room.ID, Relay: "relay-b", Seq: 1, Host: ticket.Room.Host, Online: []string{ticket.Room.Host}})
		}
		for seq := uint64(2); seq <= 4; seq++ {
			wait(ttl / 2)
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: kept.Room.ID, Relay: "relay-b", Seq: seq, Host: "acc-host", Online: []string{"acc-host"}})
		}
		// 已经过了 1.5 个 TTL：dropped 没有续期，已过期；kept 每半个 TTL 续一次，还在。
		if _, err := h.join("gt-bob", dropped.Room.ID, JoinRequest{}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("expired room: err = %v, want ErrRoomMissing", err)
		}
		listed := h.list(t, "duel")
		if len(listed) != 1 || listed[0].ID != kept.Room.ID {
			t.Fatalf("listed = %+v, want only the room kept alive by heartbeats", listed)
		}
		// 迟到的心跳不会让过期的房间复活。
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: dropped.Room.ID, Relay: "relay-b", Seq: 9, Host: "acc-alice", Online: []string{"acc-alice"}})
		if _, found, err := h.rooms.Get(context.Background(), dropped.Room.ID); err != nil || found {
			t.Fatalf("expired room after a late heartbeat: found %v, %v", found, err)
		}
		if h.redisAddr != "" {
			assertIndexHolds(t, h, []string{kept.Room.ID})
		}
		wait(ttl + 200*time.Millisecond)
		if _, err := h.join("gt-bob", kept.Room.ID, JoinRequest{}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("room after heartbeats stopped: err = %v, want ErrRoomMissing", err)
		}
	})
}

// relay 崩溃之后（不再报告），房间的寿命从最后一条报告算起：成员断线重连时反复 join、新人 join、房主改状态、
// 成员离开都会写记录，但不续命，过了 room TTL 房间就不在了。2026-11-17 多进程演练（kill -9 relay）里，
// 两个客户端按 SDK 的重连策略每隔几秒 join 一次，房间记录被续了 87 秒、一直列在大厅里，直到它们放弃重连
// （evidence/lobby-relay-multiprocess-20261117）。
func TestRejoiningDoesNotKeepARoomAliveWithoutRelayReports(t *testing.T) {
	const ttl = 1500 * time.Millisecond
	backends(t, ttl, func(t *testing.T, h *harness) {
		ctx := context.Background()
		wait := func(d time.Duration) {
			if h.redisAddr != "" {
				time.Sleep(d)
			}
			h.clock.Advance(d)
		}
		ticket := h.create(t, "gt-host", CreateRequest{})
		id := ticket.Room.ID
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: []string{"acc-host", "acc-bob"}})
		// relay 在这里崩溃：之后再没有报告。

		wait(ttl / 2)
		if _, err := h.join("gt-host", id, JoinRequest{}); err != nil { // 房主重连
			t.Fatalf("host rejoin within the ttl: %v", err)
		}
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil { // 成员重连
			t.Fatalf("member rejoin within the ttl: %v", err)
		}
		if _, err := h.join("gt-carol", id, JoinRequest{}); err != nil { // 新人从列表里看到它、加入
			t.Fatalf("newcomer join within the ttl: %v", err)
		}
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", Attrs: json.RawMessage(`{"map":"x"}`)}); err != nil {
			t.Fatalf("set state within the ttl: %v", err)
		}
		if _, err := h.svc.LeaveRoom(ctx, "gt-carol", id, LeaveRequest{Game: "party"}); err != nil {
			t.Fatalf("leave within the ttl: %v", err)
		}

		wait(ttl/2 + 100*time.Millisecond) // 最后一条报告之后已过了 room TTL
		if _, err := h.join("gt-host", id, JoinRequest{}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("host rejoin after the ttl since the last relay report: err = %v, want ErrRoomMissing", err)
		}
		if _, err := h.join("gt-bob", id, JoinRequest{}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("member rejoin after the ttl since the last relay report: err = %v, want ErrRoomMissing", err)
		}
		if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("set state after the ttl: err = %v, want ErrRoomMissing", err)
		}
		if listed := h.list(t, "duel"); len(listed) != 0 {
			t.Fatalf("listed = %+v, want no room after the ttl since the last relay report", listed)
		}
		if got := h.svc.VerifyRelayConnection(ctx, RelayVerifyRequest{Room: id, Account: "acc-bob", Relay: "relay-b"}); got.Reason != VerifyRoomNotFound {
			t.Fatalf("verify after the ttl: %+v, want %s", got, VerifyRoomNotFound)
		}
		// 迟到的报告也不让它复活。
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 2, Host: "acc-host", Online: []string{"acc-host"}})
		if _, err := h.join("gt-host", id, JoinRequest{}); !errors.Is(err, ErrRoomMissing) {
			t.Fatalf("rejoin after a late report: err = %v, want ErrRoomMissing", err)
		}
	})
}

func assertIndexHolds(t *testing.T, h *harness, want []string) {
	t.Helper()
	redis := h.rooms.(*RedisRooms)
	ids, err := redis.store.IndexDueIn(context.Background(), redis.indexKey("party", "duel"), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("index holds %v, want %v (expired entries are removed when listed)", ids, want)
	}
}

func TestNewRefusesIncompleteConfig(t *testing.T) {
	rooms, _ := NewMemoryRooms(time.Minute, nil)
	base := Config{
		Games: map[string]GameRule{"party": {MaxMembers: 4}}, Rooms: rooms, Tokens: testTokens, Relays: testRelays,
		Commands: &recordingPublisher{}, Subject: "s", RelayTokenKey: testKey,
	}
	if _, err := New(base); err != nil {
		t.Fatal(err)
	}
	broken := []func(*Config){
		func(c *Config) { c.Games = nil },
		func(c *Config) { c.Games = map[string]GameRule{"bad game": {MaxMembers: 2}} },
		func(c *Config) { c.Games = map[string]GameRule{"party": {MaxMembers: 0}} },
		func(c *Config) { c.Rooms = nil },
		func(c *Config) { c.Tokens = nil },
		func(c *Config) { c.Relays = nil },
		func(c *Config) { c.Commands = nil },
		func(c *Config) { c.Subject = " " },
		func(c *Config) { c.RelayTokenKey = []byte("short") },
	}
	for i, breakIt := range broken {
		cfg := base
		breakIt(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d: New accepted an incomplete config", i)
		}
	}
	if _, err := NewMemoryRooms(0, nil); err == nil {
		t.Error("NewMemoryRooms accepted a zero ttl")
	}
}

// 离开之后迟到的在线名单不能把人加回来（交接 A1，evidence/client-relay-flake-20261117；2026-11-17 修复前两种目录都红）。
//
// 顺序：成员经 lobby 离开之后，才处理到 relay 在他离开之前发出的一条在线名单（序号更大、名单里还有他）。
// 期望（LeaveRoom 的文档与现有用例“bob after leaving”）：离开的人席位让出，之后再 join 是新人；房间已开始时
// 新人被拒（ErrRoomStarted）。
func TestAStaleOnlineReportAfterALeaveDoesNotBringTheLeaverBack(t *testing.T) {
	// 情形一：房间还没开始时离开，随后房主开局（client/relay 的 TestRoomLifecycle 里出问题的顺序）。
	t.Run("leave_then_start", func(t *testing.T) {
		backends(t, time.Hour, func(t *testing.T, h *harness) {
			ctx := context.Background()
			id := h.create(t, "gt-host", CreateRequest{}).Room.ID
			relay := h.room(t, id).Relay.Instance
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})
			if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
				t.Fatal(err)
			}
			h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: relay, Seq: 2, Host: "acc-host", Account: "acc-bob",
				Online: []string{"acc-host", "acc-bob"}})

			if _, err := h.svc.LeaveRoom(ctx, "gt-bob", id, LeaveRequest{Game: "party"}); err != nil {
				t.Fatal(err)
			}
			t.Logf("seats right after bob's leave:            %q", seats(h.room(t, id)))

			// relay 在处理 leave 命令之前发出的报告（例如一条心跳），在 lobby 这边排在 LeaveRoom 之后。
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 3, Host: "acc-host", Online: []string{"acc-host", "acc-bob"}})
			afterStale := seats(h.room(t, id))
			t.Logf("seats after the in-flight report (seq 3):  %q", afterStale)

			if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
				t.Fatal(err)
			}
			_, err := h.join("gt-bob", id, JoinRequest{})
			t.Logf("bob joins the started room:                err = %v", err)

			// relay 处理完 leave 之后的报告。
			h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: relay, Seq: 4, Host: "acc-host", Account: "acc-bob", Reason: ReasonLeft,
				Online: []string{"acc-host"}})
			t.Logf("seats after relay's member_left (seq 4):   %q", seats(h.room(t, id)))

			if afterStale != "acc-host:online" {
				t.Errorf("the in-flight report undid the leave: seats = %q, want only the host", afterStale)
			}
			if !errors.Is(err, ErrRoomStarted) {
				t.Errorf("bob after leaving, room started: err = %v, want ErrRoomStarted", err)
			}
		})
	})

	// 情形二：房间已经开始，连上过的成员主动离开。
	t.Run("leave_a_started_room", func(t *testing.T) {
		backends(t, time.Hour, func(t *testing.T, h *harness) {
			ctx := context.Background()
			id := h.create(t, "gt-host", CreateRequest{}).Room.ID
			relay := h.room(t, id).Relay.Instance
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})
			if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
				t.Fatal(err)
			}
			h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: relay, Seq: 2, Host: "acc-host", Account: "acc-bob",
				Online: []string{"acc-host", "acc-bob"}})
			if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.LeaveRoom(ctx, "gt-bob", id, LeaveRequest{Game: "party"}); err != nil {
				t.Fatal(err)
			}
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 3, Host: "acc-host", Online: []string{"acc-host", "acc-bob"}})
			afterStale := seats(h.room(t, id))
			t.Logf("seats after the in-flight report (seq 3):  %q", afterStale)
			_, err := h.join("gt-bob", id, JoinRequest{})
			t.Logf("bob joins again:                           err = %v", err)
			h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: relay, Seq: 4, Host: "acc-host", Account: "acc-bob", Reason: ReasonLeft,
				Online: []string{"acc-host"}})
			h.lapse()
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 5, Host: "acc-host", Online: []string{"acc-host"}})
			t.Logf("seats after member_left, lapse, heartbeat: %q", seats(h.room(t, id)))

			if afterStale != "acc-host:online" {
				t.Errorf("the in-flight report undid the leave: seats = %q, want only the host", afterStale)
			}
			if !errors.Is(err, ErrRoomStarted) {
				t.Errorf("bob after leaving a started room: err = %v, want ErrRoomStarted", err)
			}
		})
	})
}

// “正在离开”的记录怎么清（交接 A1 修法 A）：更新的报告仍说他在线时，lobby 再发一次 leave 命令（leave 命令丢了也能自愈）；
// relay 报告他 member_left 后清掉；他重新 join 成功也清掉，之后 relay 的在线名单照常把他记成连上过。
func TestLeavingRecordIsResentAndCleared(t *testing.T) {
	backends(t, time.Hour, func(t *testing.T, h *harness) {
		ctx := context.Background()
		id := h.create(t, "gt-host", CreateRequest{}).Room.ID
		relay := h.room(t, id).Relay.Instance
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: relay, Seq: 2, Host: "acc-host", Account: "acc-bob", Online: []string{"acc-host", "acc-bob"}})
		if _, err := h.svc.LeaveRoom(ctx, "gt-bob", id, LeaveRequest{Game: "party"}); err != nil {
			t.Fatal(err)
		}
		if got := h.room(t, id).Leaving; fmt.Sprint(got) != "[acc-bob]" {
			t.Fatalf("leaving after bob's leave = %v", got)
		}
		// 更新的报告仍说 bob 在线：不补回席位，再发一次 leave。
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 3, Host: "acc-host", Online: []string{"acc-host", "acc-bob"}})
		leave := published{"roost.test.lobby.relay." + relay, RelayCommand{Type: CommandLeave, Room: id, Account: "acc-bob"}}
		if got := h.commands.commands(); fmt.Sprint(got) != fmt.Sprint([]published{leave, leave}) {
			t.Fatalf("relay commands = %+v, want the leave sent twice", got)
		}
		if got := seats(h.room(t, id)); got != "acc-host:online" {
			t.Fatalf("seats after a report that still lists bob = %q", got)
		}
		// relay 确认：清掉记录。
		h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: relay, Seq: 4, Host: "acc-host", Account: "acc-bob", Reason: ReasonLeft, Online: []string{"acc-host"}})
		if got := h.room(t, id).Leaving; got != nil {
			t.Fatalf("leaving after relay's member_left = %v", got)
		}

		// 再离开一次，然后重新 join（房间还开着）：记录清掉，之后的在线名单照常把他记成连上过。
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: relay, Seq: 5, Host: "acc-host", Account: "acc-bob", Online: []string{"acc-host", "acc-bob"}})
		if _, err := h.svc.LeaveRoom(ctx, "gt-bob", id, LeaveRequest{Game: "party"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		if got := h.room(t, id).Leaving; got != nil {
			t.Fatalf("leaving after bob joined again = %v", got)
		}
		h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 6, Host: "acc-host", Online: []string{"acc-host", "acc-bob"}})
		room := h.room(t, id)
		if i := room.memberIndex("acc-bob"); i < 0 || !room.Members[i].Connected {
			t.Fatalf("bob after joining again and connecting: %+v", room.Members)
		}
	})
}
