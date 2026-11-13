// 按组同步（Groups）与自身视图共用 Manager 与玩家连接：同一条 TCP 连接、同一个推送编号上，客户端按每个更新
// 自带的命名空间 + 视图键区分两路；断线重连后两路都从全量开始；Nest 事务里的 Join 在提交后才生效。

package selfsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	wiringnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
)

// roomKind 是本包测试的组实体 kind：不能与 testKind（61，玩家）、示例的 62（example_client_test.go）撞号——同一测试二进制。
const roomKind entity.EntityKind = 63

var registerRoomKind sync.Once

var roomView = entity.SyncProfile{Key: "room"}

// room 是测试里的组实体：一块记分板，命名空间 "room"，packer 只认识视图 "room"。
type room struct {
	*entity.EntityBase
	board atomic.Value
}

func (r *room) Base() *entity.EntityBase { return r.EntityBase }

func newRoom(t testing.TB, unique int64, board string) *room {
	t.Helper()
	registerRoomKind.Do(func() { entity.MustRegisterEntityKindCategory(roomKind, entity.EntityCategoryWorld) })
	id, err := entity.BuildEntityID(unique, roomKind)
	if err != nil {
		t.Fatal(err)
	}
	r := &room{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryWorld, false, roomKind)}
	r.board.Store(board)
	pack := func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		if profile.Normalize().Key != roomView.Key {
			return entity.FrozenSyncPayload{}, entity.ErrSyncViewUnknown
		}
		return entity.TakeFrozenSyncPayload(1, []byte(r.board.Load().(string))), nil
	}
	r.SetSyncState(entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, Namespace: "room", SubjectKind: uint32(roomKind),
		Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }},
	}))
	return r
}

func (r *room) setBoard(board string) {
	r.board.Store(board)
	r.Sync().MarkDirty(1)
}

// newGroupHarness 是 newHarness 加一类组实体：Groups 经 Config.Groups 交给 New，Nest Mod Init 时装到同一个 Manager。
func newGroupHarness(t testing.TB) (*harness, *Groups) {
	t.Helper()
	groups := MustNewGroups(GroupsConfig{Kind: roomKind, Profile: roomView, MaxMembers: 4})
	harnessGroups = []*Groups{groups}
	t.Cleanup(func() { harnessGroups = nil })
	return newHarness(t, nil), groups
}

// views 读下一个 Sync 推送，按 “命名空间/视图键” 返回其中每个对象的内容；remove 记为 "removed"。wait 内没有推送时 ok 为 false。
func (c *client) views(t testing.TB, wait time.Duration) (kind frame.Kind, got map[string]string, ok bool) {
	t.Helper()
	_ = c.connection.SetReadDeadline(time.Now().Add(wait))
	packet, err := wire.Read(c.connection, wire.DefaultMaxPayload)
	if err != nil {
		return 0, nil, false
	}
	if packet.Flags != wire.FlagPush|wire.FlagSync || packet.MsgID != testMessageID {
		t.Fatalf("packet %+v is not a Sync push on message %d", packet, testMessageID)
	}
	got = map[string]string{}
	decoded, err := c.receiver.Decode(packet.Payload)
	if err != nil {
		t.Fatalf("frame not applied: %v", err)
	}
	kind = decoded.Kind
	for _, object := range decoded.Objects {
		if object.Operation == frame.ObjectRemove {
			got["removed"] = fmt.Sprint(object.Ref)
			continue
		}
		for _, component := range object.Components {
			update := component.Update
			got[update.Namespace+"/"+update.Profile.Key] = string(update.Payload.BytesCopy())
		}
	}
	return kind, got, true
}

func requireViews(t testing.TB, c *client, wantKind frame.Kind, want map[string]string) {
	t.Helper()
	kind, got, ok := c.views(t, 3*time.Second)
	if !ok || kind != wantKind || len(got) != len(want) {
		t.Fatalf("frame = ok:%v %v %v, want %v %v", ok, kind, got, wantKind, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("frame %v %v, want %v %v", kind, got, wantKind, want)
		}
	}
}

func requireSilence(t testing.TB, c *client) {
	t.Helper()
	if kind, got, ok := c.views(t, 150*time.Millisecond); ok {
		t.Fatalf("unexpected push %v %v", kind, got)
	}
}

// 同一条连接上的两路同步：自身视图（player/self）只给玩家本人，房间（room/room）给全体成员；一路的变化不出现在
// 另一路里。客户端只看每个更新的命名空间与视图键，不需要第二个推送编号或第二条连接。
// 断线重连后两路都随新 epoch 的全量回来，不需要业务再 Join（policy.Group 直接挂在 self_sync 的 Manager 上时，
// 重连后房间静默缺失、组仍记着成员——这就是 Groups 存在的原因）。
func TestGroupsShareTheSelfSyncConnection(t *testing.T) {
	h, groups := newGroupHarness(t)
	alice, bob := newWallet(t, 7, 10), newWallet(t, 8, 20)
	h.access.Manager().Add(alice)
	h.access.Manager().Add(bob)
	party := newRoom(t, 900, "round=1")
	h.access.Manager().Add(party)

	// 登录之前就 Join（会话还没开）：成员关系照样记下，会话打开的那个周期一起送全量。
	for _, player := range []int64{7, 8} {
		if err := groups.Join(party.ID(), player); err != nil {
			t.Fatal(err)
		}
	}
	a, b := dial(t, h, 7), dial(t, h, 8)
	a.ack(t)
	b.ack(t)
	waitOnline(t, h, 2)
	h.flush(t)
	requireViews(t, a, frame.Full, map[string]string{"player/self": "gold=10", "room/room": "round=1"})
	requireViews(t, b, frame.Full, map[string]string{"player/self": "gold=20", "room/room": "round=1"})

	party.setBoard("round=2")
	h.flush(t)
	requireViews(t, a, frame.Delta, map[string]string{"room/room": "round=2"})
	requireViews(t, b, frame.Delta, map[string]string{"room/room": "round=2"})

	alice.setGold(11)
	h.flush(t)
	requireViews(t, a, frame.Delta, map[string]string{"player/self": "gold=11"})
	requireSilence(t, b)

	// 断线：成员关系保留，房间的变化只推给在线成员。
	_ = a.connection.Close()
	waitOnline(t, h, 1)
	h.flush(t)
	party.setBoard("round=3")
	h.flush(t)
	requireViews(t, b, frame.Delta, map[string]string{"room/room": "round=3"})
	if !groups.IsMember(party.ID(), 7) {
		t.Fatal("membership dropped on disconnect")
	}

	// 重连：两路都从全量开始。
	again := dial(t, h, 7)
	again.ack(t)
	waitOnline(t, h, 2)
	h.flush(t)
	requireViews(t, again, frame.Full, map[string]string{"player/self": "gold=11", "room/room": "round=3"})

	// 离开与解散：成员收到 remove，自身视图不受影响。
	if err := groups.Leave(party.ID(), 8); err != nil {
		t.Fatal(err)
	}
	h.flush(t)
	if _, got, ok := b.views(t, 3*time.Second); !ok || got["removed"] == "" || len(got) != 1 {
		t.Fatalf("leaving member got %v, want one remove", got)
	}
	if err := groups.Disband(party.ID()); err != nil {
		t.Fatal(err)
	}
	h.flush(t)
	if _, got, ok := again.views(t, 3*time.Second); !ok || got["removed"] == "" || len(got) != 1 {
		t.Fatalf("disband sent %v, want one remove", got)
	}
	alice.setGold(12)
	h.flush(t)
	requireViews(t, again, frame.Delta, map[string]string{"player/self": "gold=12"})
}

// Nest 事务里的 Join / Leave 在提交之后才生效：handler 失败（回滚）时成员关系不变；在 AfterCommit 回调里调用
// （事务已提交）照常立即生效。
func TestGroupsMembershipChangesFollowTheNestCommit(t *testing.T) {
	h, groups := newGroupHarness(t)
	party := newRoom(t, 901, "round=1")
	h.access.Manager().Add(party)
	engine := corenest.NewEngine(corenest.NestOptionWithGetter(h.access), corenest.NestOptionWithEntitySync(h.manager),
		corenest.NestOptionWithWorkerPools(corenest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, corenest.WorkerPoolConfig{}, corenest.WorkerPoolConfig{}))
	name := corenest.NewHandlerName("selfsync.groups.test")
	var inside atomic.Bool
	refused := errors.New("refused by the rule")
	if err := engine.RegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, args []any, _ ...corenest.HandlerOption) (any, error) {
		player, fail, nested := args[0].(int64), args[1].(bool), args[2].(bool)
		if nested {
			corenest.AfterCommit(func() {
				if err := groups.Join(party.ID(), player); err != nil {
					t.Error(err)
				}
			})
			return nil, nil
		}
		if err := groups.Join(party.ID(), player); err != nil {
			return nil, err
		}
		inside.Store(groups.IsMember(party.ID(), player))
		if fail {
			return nil, refused
		}
		return nil, nil
	}, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityMemory}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	request := func(player int64, fail, nested bool) error {
		_, err := engine.Request(context.Background(), name, party.ID(), corenest.NewParams(player, fail, nested))
		return err
	}
	if err := request(7, true, false); !errors.Is(err, refused) {
		t.Fatalf("failing handler = %v", err)
	}
	if inside.Load() || groups.IsMember(party.ID(), 7) {
		t.Fatal("a join in a rolled-back transaction took effect")
	}
	if err := request(7, false, false); err != nil {
		t.Fatal(err)
	}
	if inside.Load() {
		t.Fatal("a join took effect before its transaction committed")
	}
	if !groups.IsMember(party.ID(), 7) {
		t.Fatal("a join in a committed transaction did not take effect")
	}
	if err := request(8, false, true); err != nil {
		t.Fatal(err)
	}
	if !groups.IsMember(party.ID(), 8) {
		t.Fatal("a join from an after-commit callback was lost")
	}
}

func TestGroupsRefuseWrongIDsAndDuplicateKinds(t *testing.T) {
	playerKind(t)
	groups := MustNewGroups(GroupsConfig{Kind: roomKind, Profile: roomView})
	if err := groups.Join(newRoomID(t, 1), 7); !errors.Is(err, ErrGroupsNotAttached) {
		t.Fatalf("join before attach = %v, want ErrGroupsNotAttached", err)
	}
	walletID, _ := entity.BuildEntityID(7, testKind)
	if err := groups.Join(walletID, 7); !errors.Is(err, entitysync.ErrSubjectInvalid) {
		t.Fatalf("join with a player entity as the group = %v, want ErrSubjectInvalid", err)
	}
	if _, err := NewGroups(GroupsConfig{Kind: roomKind}); err == nil {
		t.Fatal("groups without a view key accepted")
	}
	access := entity.NewManagerAccess(entity.NewEntityManager())
	for name, config := range map[string]Config{
		"player kind":    {Entities: access, Kind: testKind, MessageID: testMessageID, Groups: []*Groups{MustNewGroups(GroupsConfig{Kind: testKind, Profile: roomView})}},
		"duplicate kind": {Entities: access, Kind: testKind, MessageID: testMessageID, Groups: []*Groups{groups, MustNewGroups(GroupsConfig{Kind: roomKind, Profile: roomView})}},
		"nil entry":      {Entities: access, Kind: testKind, MessageID: testMessageID, Groups: []*Groups{nil}},
	} {
		if _, err := New(config); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// 同一个 Groups 只能装到一个 Manager。
	first := wiringnest.NewModWithEntitySync(access, MustNew(Config{Entities: access, Kind: testKind, MessageID: testMessageID, Groups: []*Groups{groups}}).EntitySyncSetup())
	if err := first.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.EntitySync().Close(context.Background()) })
	second := wiringnest.NewModWithEntitySync(access, MustNew(Config{Entities: access, Kind: testKind, MessageID: testMessageID, Groups: []*Groups{groups}}).EntitySyncSetup())
	if err := second.Init(viper.New()); err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("one Groups attached to two managers: %v", err)
	}
	if err := groups.Join(newRoomID(t, 1), 7); err != nil {
		t.Fatalf("join on the attached groups = %v", err)
	}
}

func newRoomID(t testing.TB, unique int64) int64 {
	t.Helper()
	registerRoomKind.Do(func() { entity.MustRegisterEntityKindCategory(roomKind, entity.EntityCategoryWorld) })
	id, err := entity.BuildEntityID(unique, roomKind)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// 组同步自己的慢消费者用例（KNOWN-LIMITS“组同步的容量与独立 Gate 用例”）：组视图与自身视图共用连接与出站规则，停读的
// 成员只断开它自己——同组在读的成员照常收到每一次记分板变化、连接不受影响；停读成员的成员关系保留（业务没有 Leave），
// 重连后组视图随新 epoch 的全量回来，是断开之后的最新值。
func TestGroupsOverTCPCloseAStalledMemberOnlyAndTheReconnectGetsTheGroupFull(t *testing.T) {
	h, groups := newGroupHarness(t)
	alice, bob := newWallet(t, 7, 10), newWallet(t, 8, 20)
	h.access.Manager().Add(alice)
	h.access.Manager().Add(bob)
	party := newRoom(t, 902, "round=0")
	h.access.Manager().Add(party)
	for _, player := range []int64{7, 8} {
		if err := groups.Join(party.ID(), player); err != nil {
			t.Fatal(err)
		}
	}
	a, b := dial(t, h, 7), dial(t, h, 8)
	a.ack(t)
	b.ack(t)
	waitOnline(t, h, 2)
	h.flush(t)
	requireViews(t, a, frame.Full, map[string]string{"player/self": "gold=10", "room/room": "round=0"})
	requireViews(t, b, frame.Full, map[string]string{"player/self": "gold=20", "room/room": "round=0"})

	// bob 一直在读：记下他收到的最后一块记分板。
	var bobLast atomic.Value
	bobDone := make(chan error, 1)
	go func() {
		for {
			packet, err := wire.Read(b.connection, wire.DefaultMaxPayload)
			if err != nil {
				bobDone <- err
				return
			}
			decoded, err := b.receiver.Decode(packet.Payload)
			if err != nil {
				bobDone <- err
				return
			}
			for _, object := range decoded.Objects {
				for _, component := range object.Components {
					if component.Update.Namespace == "room" {
						bobLast.Store(string(component.Update.Payload.BytesCopy()))
					}
				}
			}
		}
	}()
	_ = b.connection.SetReadDeadline(time.Time{})

	// alice 停读；记分板每次改动带 48KiB 填充，几十帧后她的出站队列（字节上限 2MiB）连同 socket 缓冲一起满。
	padding := strings.Repeat("x", 48<<10)
	round := 0
	board := func() string { return fmt.Sprintf("round=%d;%s", round, padding) }
	for h.runtime.ActiveSessions(7) > 0 {
		if round++; round > 4096 {
			t.Fatalf("a member that stopped reading was never disconnected: %+v last=%v", h.manager.Stats(), h.manager.LastError())
		}
		party.setBoard(board())
		h.flush(t)
	}
	waitOnline(t, h, 1)
	final := board()
	for deadline := time.Now().Add(5 * time.Second); bobLast.Load() != final; {
		select {
		case err := <-bobDone:
			t.Fatalf("the member who kept reading lost the connection when another member stalled: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reading member did not get the last board (round %d)", round)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.runtime.ActiveSessions(8) != 1 || !groups.IsMember(party.ID(), 7) || !groups.IsMember(party.ID(), 8) {
		t.Fatalf("after alice stalled: bob sessions %d, members alice=%v bob=%v; want bob connected and both still members",
			h.runtime.ActiveSessions(8), groups.IsMember(party.ID(), 7), groups.IsMember(party.ID(), 8))
	}

	again := dial(t, h, 7)
	again.ack(t)
	waitOnline(t, h, 2)
	h.flush(t)
	requireViews(t, again, frame.Full, map[string]string{"player/self": "gold=10", "room/room": final})
}
