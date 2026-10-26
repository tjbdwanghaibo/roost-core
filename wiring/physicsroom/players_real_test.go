//go:build integration && physx && cgo

package physicsroom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	testInputMessageID uint32 = 0x50485902
	testFrameMessageID uint32 = 0x50485901
)

// 原 cmd/physics-viewer 的 Gate 链路由正式部件承担：客户端 TCP → Gate → NATS → GameIngress → PlayerIngress
// → 固定 owner 的 Rooms RPC → EntityHostMod 的房间（PhysX 50Hz → Nest Guard → StateDAO）→ Relay → Gate → 客户端 Chunk。
// 断言落在承诺本身：输入的加速度让受控刚体在已提交的 Entity 状态里沿 +X 移动，客户端连续收到同一 Epoch 的 Chunk 帧；
// 断开后 Relay 退出并 Leave，PlayerIngress 能在期限内排空。
func TestRealGateInputMovesRoomBodyAndStreamsChunks(t *testing.T) {
	url := os.Getenv("ROOST_PHYSICS_TEST_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, rooms := startRoomOwner(t, ctx, url)
	instance, err := rooms.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const body uint64 = 1
	ref, err := rooms.Create(ctx, domain.Spec{Instance: instance, ID: "ingress", LifetimeSeconds: 30, Bodies: []physx.Body{
		{ID: 100, Shape: physx.Box, Position: physx.Vec3{0, -.5, 0}, Dimensions: physx.Vec3{25, .5, 25}},
		{ID: body, Shape: physx.Sphere, Position: physx.Vec3{0, .5, 0}, Dimensions: physx.Vec3{.5}, Mass: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}

	players, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: testInputMessageID, FrameMessageID: testFrameMessageID, Seat: func(context.Context, gateway.Binding) (domain.Ref, uint64, error) {
		return ref, body, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if players.Ready() {
		t.Fatal("ready before attach")
	}
	game, addr := startGateForPlayers(t, url, players)
	if err = players.Attach(rooms, game); err != nil {
		t.Fatal(err)
	}

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if err = wire.Write(conn, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte("ticket")}}, 8192); err != nil {
		t.Fatal(err)
	}
	if ack, err := wire.Read(conn, 8192); err != nil || ack.MsgID != 0 || ack.Seq != 1 {
		t.Fatalf("auth ack=%v err=%v", ack, err)
	}

	// 输入每 50ms 一条（InputTTL 250ms 内持续施力），读循环在主协程。
	inputCtx, stopInput := context.WithCancel(ctx)
	inputDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for seq := uint32(2); ; seq++ {
			payload := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), uint64(seq))
			payload = protowire.AppendFixed32(protowire.AppendTag(payload, 2, protowire.Fixed32Type), math.Float32bits(10))
			if err := wire.Write(conn, []*wire.Packet{{MsgID: testInputMessageID, Seq: seq, Payload: payload}}, 8192); err != nil {
				inputDone <- err
				return
			}
			select {
			case <-inputCtx.Done():
				inputDone <- nil
				return
			case <-ticker.C:
			}
		}
	}()
	var replica gamephysics.RoomReplica
	frames, inputs := 0, 0
	moved := false
	for frames < 5 || inputs == 0 || !moved {
		packet, err := wire.Read(conn, 1<<20)
		if err != nil {
			t.Fatalf("frames=%d inputs=%d moved=%v: %v", frames, inputs, moved, err)
		}
		switch packet.MsgID {
		case testInputMessageID:
			if packet.Flags&wire.FlagPush != 0 {
				t.Fatalf("input reply flags=%d", packet.Flags)
			}
			inputs++
		case testFrameMessageID:
			if packet.Flags != wire.FlagSync|wire.FlagPush {
				t.Fatalf("frame flags=%d", packet.Flags)
			}
			if err = replica.Apply(packet.Payload); err != nil {
				t.Fatal(err)
			}
			if replica.Epoch != ref.Epoch {
				t.Fatalf("epoch=%d want %d", replica.Epoch, ref.Epoch)
			}
			if _, ok := replica.Objects[body]; !ok {
				t.Fatal("controlled body missing from chunk frame")
			}
			frames++
		default:
			t.Fatalf("unexpected packet %+v", packet)
		}
		// 首帧提交前 Host 里还没有这个房间的 Entity。
		committed, err := owner.Host().Snapshot(ctx, ref)
		if errors.Is(err, domain.ErrMissing) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range committed.States {
			if state.ID == body && state.Position[0] > 1 {
				moved = true
			}
		}
	}
	stopInput()
	if err = <-inputDone; err != nil {
		t.Fatal(err)
	}
	// 断开连接：OnClosed 取消 Relay，Relay 负责 Leave；Close 在期限内排空。
	_ = conn.Close()
	drain, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer drainCancel()
	if err = players.Close(drain); err != nil {
		t.Fatal(err)
	}
	if players.Ready() {
		t.Fatal("ready after close")
	}
	if _, err = players.Dispatch(ctx, nil, testInputMessageID, 9, wire.PayloadProtobuf, nil); !errors.Is(err, domain.ErrRequestInvalid) && !errors.Is(err, domain.ErrStopping) {
		t.Fatalf("closed ingress accepted input: %v", err)
	}
	if err = rooms.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	t.Logf("frames=%d input_replies=%d tick=%d", frames, inputs, replica.Tick)
}

// 原 cmd/physics-viewer 的 UGC 关卡链路改由正式部件承担：客户端经 Gate 先取内容握手（关卡字节、SHA-256、受控刚体），
// 再发带 grab_target 的输入；输入应答回成员代际，RP2 私有头里的采样确认带同一代际，运动学平台随帧出现。
func TestRealGateUGCLevelAndInput(t *testing.T) {
	url := os.Getenv("ROOST_PHYSICS_TEST_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	const contentMessageID uint32 = 0x50485903
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, rooms := startRoomOwner(t, ctx, url)
	instance, err := rooms.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	level := gamephysics.ObstacleLevel()
	content, hash, err := level.Content()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := rooms.Create(ctx, domain.Spec{Instance: instance, ID: "ugc", LifetimeSeconds: 30, Bodies: level.Bodies, Motions: level.Motions, Joints: level.Joints})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rooms.Destroy(context.Background(), ref) }()
	body := level.Players[0]
	players, err := NewPlayerIngress(PlayerIngressConfig{InputMessageID: testInputMessageID, FrameMessageID: testFrameMessageID,
		Seat:             func(context.Context, gateway.Binding) (domain.Ref, uint64, error) { return ref, body, nil },
		ContentMessageID: contentMessageID,
		Content: func(context.Context, gateway.Binding) (RoomContent, error) {
			return RoomContent{Data: content, SHA256: hash, Body: body}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	game, addr := startGateForPlayers(t, url, players)
	if err = players.Attach(rooms, game); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		drain, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := players.Close(drain); err != nil {
			t.Error(err)
		}
	})
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	send := func(id, seq uint32, payload []byte) {
		t.Helper()
		if err := wire.Write(conn, []*wire.Packet{{MsgID: id, Seq: seq, Payload: payload}}, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	read := func() *wire.Packet {
		t.Helper()
		p, err := wire.Read(conn, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	send(0, 1, []byte("ticket"))
	if p := read(); p.MsgID != 0 || p.Seq != 1 {
		t.Fatalf("auth ack=%+v", p)
	}
	send(contentMessageID, 2, nil)
	p := read()
	if p.MsgID != contentMessageID || p.Seq != 2 {
		t.Fatalf("content reply=%+v", p)
	}
	data := p.Payload
	_, _, n := protowire.ConsumeTag(data)
	got, m := protowire.ConsumeBytes(data[n:])
	data = data[n+m:]
	_, _, n = protowire.ConsumeTag(data)
	gotHash, m := protowire.ConsumeString(data[n:])
	data = data[n+m:]
	_, _, n = protowire.ConsumeTag(data)
	gotBody, m := protowire.ConsumeVarint(data[n:])
	if m < 0 || gotBody != body || gotHash != hash {
		t.Fatalf("content body=%d hash=%s", gotBody, gotHash)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != hash {
		t.Fatal("content hash mismatch")
	}
	if decoded, err := gamephysics.DecodeLevel(got); err != nil || decoded.Name != level.Name {
		t.Fatalf("content decode: %v", err)
	}
	// sequence=1，按住抓取 3 号球（与受控球相距 1m，由服务器判定）。
	payload := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1)
	payload = protowire.AppendVarint(protowire.AppendTag(payload, 5, protowire.VarintType), 3)
	send(testInputMessageID, 3, payload)
	var replica gamephysics.RoomReplica
	var generation uint64
	sampled, moving := false, false
	for !sampled || !moving || generation == 0 {
		p = read()
		switch p.MsgID {
		case testInputMessageID:
			_, _, n := protowire.ConsumeTag(p.Payload)
			v, m := protowire.ConsumeVarint(p.Payload[max(n, 0):])
			if n < 0 || m < 0 || v == 0 {
				t.Fatalf("input ack=%x", p.Payload)
			}
			generation = v
		case testFrameMessageID:
			if err = replica.Apply(p.Payload); err != nil {
				t.Fatal(err)
			}
			if replica.Epoch != ref.Epoch {
				t.Fatalf("epoch=%d want %d", replica.Epoch, ref.Epoch)
			}
			if _, ok := replica.States[body]; ok && replica.Ack.Sequence == 1 {
				if generation != 0 && replica.Ack.Generation != generation {
					t.Fatalf("ack generation=%d want %d", replica.Ack.Generation, generation)
				}
				sampled = true
			}
			_, moving = replica.States[4]
		default:
			t.Fatalf("unexpected packet %+v", p)
		}
	}
	t.Logf("UGC content=%s bodies=%d tick=%d generation=%d", hash, len(level.Bodies), replica.Tick, generation)
}

// startRoomOwner 在私有命名空间里按 owner 进程的正式装配起 EntityHostMod 与 Server，返回固定 owner 的房间客户端。
func startRoomOwner(t *testing.T, ctx context.Context, url string) (*EntityHostMod, domain.Rooms) {
	t.Helper()
	asm, err := driver.Assemble(fnats.DefaultConfig(url), driver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(asm.Client.Close)
	prefix := fmt.Sprintf("roost.physics.ingress.n%d", time.Now().UnixNano())
	makeBus := func(sid int32) *bus.Bus {
		b := bus.New(asm.Client, asm.RPC, nil, bus.Config{Sid: sid, SvcType: domain.ServiceType, Prefix: prefix, WorkerNum: 4, QueueCap: 64})
		if err := b.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := b.StopWithContext(ctx); err != nil {
				t.Error(err)
			}
		})
		return b
	}
	ownerBus, callerBus := makeBus(7901), makeBus(7902)

	// owner 进程的正式装配：EntityHostMod 按声明读配置并提供能力，Server 在 bus 上登记生成的 handler。
	cfg := viper.New()
	cfg.Set("physicsroom.entity_kind", 242)
	cfg.Set("physicsroom.entity_category", 1)
	cfg.Set("physicsroom.max_rooms", 2)
	owner := NewEntityHostMod()
	if err = owner.Init(cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	if err = registry.Register(mods.ModBus, bus.IBus(ownerBus)); err != nil {
		t.Fatal(err)
	}
	if err = owner.Provide(registry); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := owner.StopWithContext(ctx); err != nil {
			t.Error(err)
		}
	})
	server := NewServer()
	if err = server.Init(registry); err != nil {
		t.Fatal(err)
	}
	if err = asm.Client.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}

	rooms, err := domain.NewOwnerClient(callerBus, 7901, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return owner, rooms
}

// startGateForPlayers 在私有命名空间里起真实 Gate、GameIngress 与 TCP 监听，GameIngress 的分发、准入与连接生命周期
// 全部交给 PlayerIngress；与 wiring/gate 的真实夹具同样构造，只换成本包的接线。
func startGateForPlayers(t *testing.T, url string, players *PlayerIngress) (*gateway.GameIngress, string) {
	t.Helper()
	cfg := gateway.DefaultConfig()
	cfg.Namespace = fmt.Sprintf("roost.physics.gate.n%d", time.Now().UnixNano())
	gateID := gateway.ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gameID := gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	client := func(role string, id gateway.ProcessIdentity) *driver.Client {
		prefix, _ := gateway.InboxPrefix(cfg.Namespace, role, id)
		c, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{InboxPrefix: prefix, ReconnectBufferBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	gateClient, gameClient := client("gate", gateID), client("game", gameID)
	auth := gateway.AuthenticatorFunc(func(context.Context, string, net.Addr) (gateway.Principal, error) {
		return gateway.Principal{PlayerID: 7, ServerID: 22, SessionID: "session-7"}, nil
	})
	match := func(_ context.Context, role string, id gateway.ProcessIdentity) (bool, error) {
		return (role == "gate" && id == gateID) || (role == "game" && id == gameID), nil
	}
	game, err := gateway.NewGameIngress(cfg, gameID, gateway.GameDependencies{Client: gameClient, Authenticator: auth, QueueFactory: gatewiring.AsyncQueueFactory, Matches: match,
		Dispatch: players.Dispatch, Ready: players.Ready, OnActive: players.OnActive, OnClosed: players.OnClosed})
	if err != nil {
		t.Fatal(err)
	}
	if err = game.Start(); err != nil {
		t.Fatal(err)
	}
	gate, err := gateway.NewGate(cfg, gateID, gateway.GateDependencies{Client: gateClient, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, ResolveGame: func(context.Context, int32) (gateway.ProcessIdentity, error) { return gameID, nil }, Matches: match})
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.Start(); err != nil {
		t.Fatal(err)
	}
	tcp := gateway.DefaultTCPConfig()
	tcp.Enabled = true
	tcp.Addr = "127.0.0.1:0"
	tcp.MaxPayloadBytes = uint32(cfg.MaxPayloadBytes)
	server, err := gateway.NewTCPServer(tcp, gate.Forward, auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.ConnectHandshake(gate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, c := range []*driver.Client{gateClient, gameClient} {
		if err = c.FlushContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := gate.Stop(ctx); err != nil {
			t.Error(err)
		}
		if err := server.Stop(ctx); err != nil {
			t.Error(err)
		}
		if err := game.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return game, server.Addr().String()
}
