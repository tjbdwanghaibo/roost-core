package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	roomwiring "github.com/tjbdwanghaibo/roost-core/wiring/physicsroom"
	"google.golang.org/protobuf/encoding/protowire"
)

const roomOwnerSID int32 = 7801
const inputMessageID uint32 = 0x50485902

// 两个独立进程共用固定本地命名空间；只接 loopback，业务部署应另行分配 SID 和认证入口。
func roomBus(url string, sid int32) (*bus.Bus, func(), error) {
	a, err := driver.Assemble(fnats.DefaultConfig(url), driver.ClientOptions{})
	if err != nil {
		return nil, nil, err
	}
	b := bus.New(a.Client, a.RPC, nil, bus.Config{Sid: sid, SvcType: domain.ServiceType, Prefix: "roost.physics.local", WorkerNum: 8, QueueCap: 256})
	if err = b.Start(); err != nil {
		a.Client.Close()
		return nil, nil, err
	}
	return b, func() { _ = b.StopWithContext(context.Background()); a.Client.Close() }, nil
}

func runPhysicsService(ctx context.Context, url string) error {
	if err := entity.RegisterEntityKindCategory(240, 1); err != nil {
		return err
	}
	host, err := roomwiring.NewEntityHost(240, 1, 64, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 128}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}))
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	cfg := domain.DefaultConfig(func(ctx context.Context, ref domain.Ref, f gamephysics.Frame) error {
		err := host.Commit(ctx, ref, f)
		if err != nil {
			slog.Error("room commit failed", "room", ref.ID, "tick", f.Tick, "error", err)
		} else if f.Tick == 1 || f.Tick%50 == 0 {
			slog.Debug("room entity committed", "room", ref.ID, "tick", f.Tick, "bodies", len(f.States))
		}
		return err
	})
	cfg.Closed = func(ref domain.Ref) { host.Forget(ref); slog.Debug("room entity released", "room", ref.ID) }
	service, err := domain.New(cfg)
	if err != nil {
		return err
	}
	defer service.Shutdown(context.Background())
	b, closeBus, err := roomBus(url, roomOwnerSID)
	if err != nil {
		return err
	}
	defer closeBus()
	if err = domain.RegisterHandlers(b, service); err != nil {
		return err
	}
	slog.Info("physics service ready", "sid", roomOwnerSID, "physics_hz", 50, "transport", "NATS MessagePack")
	<-ctx.Done()
	return nil
}

type roomConnection struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	binding gateway.Binding
	member  domain.Member
	joined  bool
}

func runPhysicsGate(ctx context.Context, addr, url string) error {
	caller, closeBus, err := roomBus(url, 7802)
	if err != nil {
		return err
	}
	defer closeBus()
	rooms, err := domain.NewOwnerClient(caller, roomOwnerSID, 2*time.Second)
	if err != nil {
		return err
	}
	instance, err := rooms.Describe(ctx)
	if err != nil {
		return fmt.Errorf("start Physics Service before Physics Gate: %w", err)
	}
	ref, err := rooms.Create(ctx, domain.Spec{Instance: instance, ID: "unity-" + roomToken(), Bodies: gamephysics.CrowdScene(), LifetimeSeconds: 3600})
	if err != nil {
		return err
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := rooms.Destroy(stop, ref); err != nil {
			slog.Error("room destroy failed", "error", err)
		}
	}()
	cfg := gateway.DefaultConfig()
	cfg.Namespace = "roost.physics.gate"
	gateID := gateway.ProcessIdentity{ServerID: 11, Incarnation: roomToken()}
	gameID := gateway.ProcessIdentity{ServerID: 22, Incarnation: roomToken()}
	makeClient := func(role string, id gateway.ProcessIdentity) (*driver.Client, error) {
		prefix, err := gateway.InboxPrefix(cfg.Namespace, role, id)
		if err != nil {
			return nil, err
		}
		return driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{InboxPrefix: prefix, ReconnectBufferBytes: -1})
	}
	gateClient, err := makeClient("gate", gateID)
	if err != nil {
		return err
	}
	defer gateClient.Close()
	gameClient, err := makeClient("game", gameID)
	if err != nil {
		return err
	}
	defer gameClient.Close()
	auth := gateway.AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
		slot, err := strconv.Atoi(strings.TrimPrefix(ticket, "physics-slot-"))
		if err != nil || ticket != "physics-slot-"+strconv.Itoa(slot) || slot < 1 || slot > 20 {
			return gateway.Principal{}, gateway.ErrUnauthenticated
		}
		return gateway.Principal{PlayerID: int64(slot), ServerID: 22, SessionID: ticket}, nil
	})
	matches := func(_ context.Context, role string, id gateway.ProcessIdentity) (bool, error) {
		return role == "gate" && id == gateID || role == "game" && id == gameID, nil
	}
	var mu sync.Mutex
	connections := make(map[gateway.Binding]*roomConnection)
	var relays sync.WaitGroup
	var game *gateway.GameIngress
	deps := gateway.GameDependencies{Client: gameClient, Authenticator: auth, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: matches}
	deps.OnActive = func(binding gateway.Binding, _ uint64) error {
		child, cancel := context.WithCancel(ctx)
		mu.Lock()
		connections[binding] = &roomConnection{ctx: child, cancel: cancel, binding: binding}
		mu.Unlock()
		slog.Debug("gate binding active", "player", binding.PlayerID, "session", binding.SessionID)
		return nil
	}
	deps.OnClosed = func(binding gateway.Binding, _ uint64) {
		mu.Lock()
		c := connections[binding]
		delete(connections, binding)
		mu.Unlock()
		if c != nil {
			c.cancel()
		}
		slog.Debug("gate binding closed", "player", binding.PlayerID)
	}
	deps.Dispatch = func(callCtx context.Context, session gateway.Session, id, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		if id != inputMessageID || kind != wire.PayloadProtobuf {
			return nil, domain.ErrRequestInvalid
		}
		input, err := decodeRoomInput(payload)
		if err != nil {
			return nil, err
		}
		binding, _, ok := game.SessionReceiver(session.Principal().SessionID)
		if !ok {
			return nil, gateway.ErrBindingStale
		}
		mu.Lock()
		c := connections[binding]
		mu.Unlock()
		if c == nil {
			return nil, gateway.ErrBindingStale
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if err = c.ctx.Err(); err != nil {
			return nil, err
		}
		if !c.joined {
			c.member, err = rooms.Join(callCtx, ref, binding, uint64(gamephysics.CrowdCapsules)+uint64(binding.PlayerID))
			if err != nil {
				return nil, err
			}
			c.joined = true
			relays.Add(1)
			go func() {
				defer relays.Done()
				err := roomwiring.Relay(c.ctx, rooms, game, c.binding, c.member, frameMessageID)
				if err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("room relay ended", "player", c.binding.PlayerID, "error", err)
					_ = session.Close(err)
				}
			}()
			slog.Debug("room joined via RPC", "room", ref.ID, "player", binding.PlayerID)
		}
		if err = rooms.Submit(callCtx, c.member, input); err != nil {
			return nil, err
		}
		if input.Jump || input.Sequence%20 == 0 {
			slog.Debug("room input accepted via RPC", "player", binding.PlayerID, "sequence", input.Sequence, "acceleration", input.Acceleration, "jump", input.Jump)
		}
		return &gateway.TCPResponse{MessageID: id, Sequence: seq}, nil
	}
	game, err = gateway.NewGameIngress(cfg, gameID, deps)
	if err != nil {
		return err
	}
	if err = game.Start(); err != nil {
		return err
	}
	defer func() {
		_ = game.Stop(context.Background())
		mu.Lock()
		for _, c := range connections {
			c.cancel()
		}
		mu.Unlock()
		relays.Wait()
	}()
	gate, err := gateway.NewGate(cfg, gateID, gateway.GateDependencies{Client: gateClient, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, ResolveGame: func(context.Context, int32) (gateway.ProcessIdentity, error) { return gameID, nil }, Matches: matches})
	if err != nil {
		return err
	}
	if err = gate.Start(); err != nil {
		return err
	}
	defer gate.Stop(context.Background())
	tcp := gateway.DefaultTCPConfig()
	tcp.Enabled = true
	tcp.Addr = addr
	tcp.MaxPayloadBytes = uint32(cfg.MaxPayloadBytes)
	server, err := gateway.NewTCPServer(tcp, gate.Forward, auth)
	if err != nil {
		return err
	}
	if err = server.ConnectHandshake(gate); err != nil {
		return err
	}
	flushCtx, flushCancel := context.WithTimeout(ctx, 5*time.Second)
	defer flushCancel()
	for _, client := range []*driver.Client{gateClient, gameClient} {
		if err = client.FlushContext(flushCtx); err != nil {
			return err
		}
	}
	if err = server.Start(); err != nil {
		return err
	}
	defer server.Stop(context.Background())
	slog.Info("physics gate ready", "address", server.Addr(), "room", ref.ID, "chain", "Unity -> Gate -> NATS -> GameIngress -> NATS RPC -> PhysicsService")
	<-ctx.Done()
	return nil
}

// RoomInput PB: uint64 sequence=1; float acceleration_x=2; float acceleration_z=3; bool jump=4.
func decodeRoomInput(data []byte) (domain.Input, error) {
	var in domain.Input
	seen := uint8(0)
	for len(data) > 0 {
		field, typ, n := protowire.ConsumeTag(data)
		if n < 0 || field < 1 || field > 4 {
			return in, domain.ErrRequestInvalid
		}
		data = data[n:]
		bit := uint8(1 << field)
		if seen&bit != 0 {
			return in, domain.ErrRequestInvalid
		}
		seen |= bit
		if field == 2 || field == 3 {
			if typ != protowire.Fixed32Type {
				return in, domain.ErrRequestInvalid
			}
			v, n := protowire.ConsumeFixed32(data)
			if n < 0 {
				return in, domain.ErrRequestInvalid
			}
			data = data[n:]
			if field == 2 {
				in.Acceleration[0] = math.Float32frombits(v)
			} else {
				in.Acceleration[2] = math.Float32frombits(v)
			}
		} else {
			if typ != protowire.VarintType {
				return in, domain.ErrRequestInvalid
			}
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return in, domain.ErrRequestInvalid
			}
			data = data[n:]
			if field == 1 {
				in.Sequence = v
			} else {
				if v > 1 {
					return in, domain.ErrRequestInvalid
				}
				in.Jump = v == 1
			}
		}
	}
	if in.Sequence == 0 {
		return in, domain.ErrRequestInvalid
	}
	return in, nil
}
