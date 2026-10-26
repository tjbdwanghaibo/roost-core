// physicsroom-local 是物理房间在本机的标准进程入口：同一个二进制按子命令起三个 App 服务，全部用正式 Mod 装配。
//
//	physicsroom  房间 owner：NATS Mod + EntityHostMod（PhysX 房间、房间 Entity、Nest 提交）+ Rooms RPC Server
//	game         Game 侧接入：GameIngressMod + 固定 owner 的 ClientMod + PlayerIngress（Join / Relay / Submit）
//	gate         独立 Gate：GateMod（TCP 监听 127.0.0.1:19093，经 etcd 发现 game、经 NATS 转发）
//
// 本机专用的只有两处，都在本文件：physics-slot-N 票据的 loopback 鉴权，以及首个输入时按需创建的
// CrowdScene 房间（Seat）。正式部署由业务提供账号鉴权、开房与选座，其余接线照搬。
// 依赖：NATS、Redis（App 单实例锁，Gate 接线要求）与 etcd（Game 候选发现），地址见 configs/。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	kitetcd "github.com/tjbdwanghaibo/roost-core/wiring/etcd"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	roomwiring "github.com/tjbdwanghaibo/roost-core/wiring/physicsroom"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"
)

// 客户端协议与本机拓扑：Unity 的 client/physics-viewer 用同样的值。三个 sid 要与 configs/ 里的 sid 一致，
// game 服务启动时核对自己的 sid，Gate 按票据里的 ServerID 路由到它。
const (
	ownerSID       int32  = 7801
	gameSID        int32  = 22
	gateAddr              = "127.0.0.1:19093"
	frameMessageID uint32 = 0x50485901
	inputMessageID uint32 = 0x50485902
	roomLifetime          = 3600
	rpcTimeout            = 2 * time.Second
)

func main() {
	players := newSeats()
	ingress, err := roomwiring.NewPlayerIngress(roomwiring.PlayerIngressConfig{InputMessageID: inputMessageID, FrameMessageID: frameMessageID, Seat: players.seat})
	if err != nil {
		slog.Error("physicsroom-local", "error", err)
		os.Exit(1)
	}
	a := app.New("physicsroom-local", buildinfo.VersionString()).Singleton(kitredis.SingletonStore)
	a.RegisterServer(app.ServiceName(domain.ServiceType), roomwiring.NewServer(),
		kitnats.NewNatsMod(nil), roomwiring.NewEntityHostMod())
	a.RegisterServer("game", &gameService{seats: players, ingress: ingress},
		kitnats.NewNatsMod(nil), kitetcd.NewEtcdMod(kitetcd.WithoutServiceRegistration()),
		roomwiring.NewClientMod(ownerSID, rpcTimeout),
		gatewiring.NewGameIngressMod(ingress.Dispatch, nil, gameOptions(ingress)), ingress)
	a.RegisterServer("gate", idleService("gate"),
		kitnats.NewNatsMod(nil), kitetcd.NewEtcdMod(kitetcd.WithoutServiceRegistration()), gatewiring.NewGateMod(gateOptions()))
	if err := a.Execute(); err != nil {
		slog.Error("physicsroom-local", "error", err)
		os.Exit(1)
	}
}

// slotAuth 只接受 physics-slot-N（N=1..CrowdPlayers），PlayerID 就是 N，路由到本机 game。仅用于 loopback 调试。
var slotAuth = gateway.AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
	slot, err := strconv.Atoi(strings.TrimPrefix(ticket, "physics-slot-"))
	if err != nil || ticket != "physics-slot-"+strconv.Itoa(slot) || slot < 1 || slot > gamephysics.CrowdPlayers {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	return gateway.Principal{PlayerID: int64(slot), ServerID: gameSID, SessionID: ticket}, nil
})

func gateOptions() gatewiring.Options {
	options := gatewiring.DefaultOptions()
	options.Authenticator = slotAuth
	options.RegisterDiscovery = true
	options.AdvertiseAddr = gateAddr
	options.Ready = func() bool { return true }
	options.TCP.Enabled = true
	options.TCP.Addr = gateAddr
	return options
}

func gameOptions(ingress *roomwiring.PlayerIngress) gatewiring.Options {
	options := gatewiring.DefaultOptions()
	options.Authenticator = slotAuth
	options.RegisterDiscovery = true
	options.AdvertiseAddr = "127.0.0.1"
	options.Ready = ingress.Ready
	options.OnActive = ingress.OnActive
	options.OnClosed = ingress.OnClosed
	return options
}

// seats 是本机的开房与选座：首个输入时创建一个 CrowdScene 房间，slot N 控制第 CrowdCapsules+N 个刚体。
type seats struct {
	mu    sync.Mutex
	rooms domain.Rooms
	room  domain.Ref
}

func newSeats() *seats { return &seats{} }

func (s *seats) seat(ctx context.Context, binding gateway.Binding) (domain.Ref, uint64, error) {
	if binding.PlayerID < 1 || binding.PlayerID > gamephysics.CrowdPlayers {
		return domain.Ref{}, 0, domain.ErrRequestInvalid
	}
	body := uint64(gamephysics.CrowdCapsules) + uint64(binding.PlayerID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rooms == nil {
		return domain.Ref{}, 0, domain.ErrStopping
	}
	if s.room.ID != "" {
		return s.room, body, nil
	}
	// Describe 取 owner 代际并随这次 Create 保留；失败时下一次输入重新开房，不复用未知结果的请求。
	instance, err := s.rooms.Describe(ctx)
	if err != nil {
		return domain.Ref{}, 0, fmt.Errorf("physicsroom owner (sid %d) unavailable: %w", ownerSID, err)
	}
	ref, err := s.rooms.Create(ctx, domain.Spec{Instance: instance, ID: "unity-" + token(), Bodies: gamephysics.CrowdScene(), LifetimeSeconds: roomLifetime})
	if err != nil {
		return domain.Ref{}, 0, err
	}
	s.room = ref
	slog.Info("physics room created", "room", ref.ID, "owner", ownerSID, "lifetime_seconds", roomLifetime)
	return ref, body, nil
}

// destroy 在 game 停止时结束本机创建的房间；PlayerIngress 已先排空 Relay。
func (s *seats) destroy(ctx context.Context) error {
	s.mu.Lock()
	rooms, room := s.rooms, s.room
	s.rooms, s.room = nil, domain.Ref{}
	s.mu.Unlock()
	if rooms == nil || room.ID == "" {
		return nil
	}
	err := rooms.Destroy(ctx, room)
	slog.Info("physics room destroyed", "room", room.ID, "error", err)
	return err
}

type gameService struct {
	seats   *seats
	ingress *roomwiring.PlayerIngress
}

func (*gameService) Name() app.ServiceName { return "game" }
func (g *gameService) Init(r *app.Registry) error {
	self, ok := app.Lookup[app.SingletonIncarnation](r, app.ModSingletonIncarnation)
	if !ok || self.Sid != gameSID {
		return fmt.Errorf("game must run with singleton enabled and sid %d (Gate routes physics-slot tickets there)", gameSID)
	}
	rooms, ok := app.Lookup[domain.Rooms](r, domain.CapabilityName)
	if !ok {
		return errors.New("game: physicsroom client missing")
	}
	g.seats.mu.Lock()
	g.seats.rooms = rooms
	g.seats.mu.Unlock()
	slog.Info("physics game ready", "sid", gameSID, "owner", ownerSID, "chain", "Gate -> NATS -> GameIngress -> Rooms RPC -> physicsroom")
	return nil
}
func (*gameService) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (g *gameService) Shutdown(ctx context.Context) error {
	return errors.Join(g.ingress.Close(ctx), g.seats.destroy(ctx))
}

type idleService app.ServiceName

func (s idleService) Name() app.ServiceName { return app.ServiceName(s) }
func (s idleService) Init(*app.Registry) error {
	slog.Info("physics "+string(s)+" ready", "address", gateAddr)
	return nil
}
func (idleService) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (idleService) Shutdown(context.Context) error  { return nil }

func token() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
