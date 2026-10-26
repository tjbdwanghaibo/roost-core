// physicsroom-local 是物理房间在本机的标准进程入口：同一个二进制按子命令起三个 App 服务，全部用正式 Mod 装配。
//
//	physicsroom  房间 owner：NATS Mod + EntityHostMod（PhysX 房间、房间 Entity、Nest 提交）+ Rooms RPC Server
//	game         Game 侧接入：GameIngressMod + 固定 owner 的 ClientMod + PlayerIngress（Join / Relay / Submit）
//	gate         独立 Gate：GateMod（TCP 监听 127.0.0.1:19093，经 etcd 发现 game、经 NATS 转发）
//
// 本机专用的只有三处，都在本文件：physics-slot-N 票据的 loopback 鉴权、首个输入时按需创建的房间（Seat），
// 以及房间内容握手（Content）。房间内容缺省是 CrowdScene；game 子命令加 --level 时改用校验过的 UGC 关卡 JSON
// （export-level 子命令导出可编辑的示例）。正式部署由业务提供账号鉴权、开房、选座与内容发布，其余接线照搬。
// 依赖：NATS、Redis（App 单实例锁，Gate 接线要求）与 etcd（Game 候选发现），地址见 configs/。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
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
	ownerSID         int32  = 7801
	gameSID          int32  = 22
	gateAddr                = "127.0.0.1:19093"
	frameMessageID   uint32 = 0x50485901
	inputMessageID   uint32 = 0x50485902
	contentMessageID uint32 = 0x50485903
	roomLifetime            = 3600
	rpcTimeout              = 2 * time.Second
)

func main() {
	players := newSeats()
	ingress, err := roomwiring.NewPlayerIngress(roomwiring.PlayerIngressConfig{InputMessageID: inputMessageID, FrameMessageID: frameMessageID, Seat: players.seat,
		ContentMessageID: contentMessageID, Content: players.content})
	if err != nil {
		slog.Error("physicsroom-local", "error", err)
		os.Exit(1)
	}
	a := app.New("physicsroom-local", buildinfo.VersionString()).Singleton(kitredis.SingletonStore)
	a.RootCmd().PersistentFlags().StringVar(&players.levelPath, "level", "", "game only: validated UGC level JSON; empty uses the CrowdScene level")
	a.RootCmd().AddCommand(&cobra.Command{Use: "export-level <file>", Short: "write the sample obstacle level JSON and exit", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			data, _, err := gamephysics.ObstacleLevel().Content()
			if err != nil {
				return err
			}
			return os.WriteFile(args[0], data, 0o644)
		}})
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

// slotAuth 只接受 physics-slot-N（N=1..CrowdPlayers，也是 UGC 关卡的槽数上限），PlayerID 就是 N，路由到本机 game。
// 仅用于 loopback 调试；Gate 进程不读关卡，超出所选关卡槽数的票据由 game 的 Seat / Content 拒绝。
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

// seats 是本机的开房、选座与内容：game 启动时固定一个关卡（缺省 CrowdScene，slot N 控制第 CrowdCapsules+N 个刚体；
// --level 时 slot N 控制关卡 Players[N-1]），首个输入时按它创建一个房间。关卡在进程内不变，换关卡要重启 game 开新房。
type seats struct {
	levelPath string // --level，Init 前由命令行写入

	mu    sync.Mutex
	rooms domain.Rooms
	room  domain.Ref
	level gamephysics.Level
	data  []byte
	hash  string
}

func newSeats() *seats { return &seats{} }

// load 在 game 服务 Init 时调用一次：读取并校验关卡，固定内容字节与摘要。
func (s *seats) load() error {
	level := gamephysics.Level{Version: 1, Name: "crowd", Revision: 1, Bodies: gamephysics.CrowdScene()}
	for i := 1; i <= gamephysics.CrowdPlayers; i++ {
		level.Players = append(level.Players, uint64(gamephysics.CrowdCapsules+i))
	}
	if s.levelPath != "" {
		file, err := os.Open(s.levelPath)
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, gamephysics.MaxLevelBytes+1))
		if err != nil {
			return err
		}
		if level, err = gamephysics.DecodeLevel(data); err != nil {
			return fmt.Errorf("level %s: %w", s.levelPath, err)
		}
	}
	content, hash, err := level.Content()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.level, s.data, s.hash = level, content, hash
	s.mu.Unlock()
	slog.Info("physics level loaded", "name", level.Name, "revision", level.Revision, "sha256", hash, "slots", len(level.Players))
	return nil
}

// body 返回 slot 对应的受控刚体；调用方持有 s.mu。
func (s *seats) body(binding gateway.Binding) (uint64, error) {
	if binding.PlayerID < 1 || binding.PlayerID > int64(len(s.level.Players)) {
		return 0, domain.ErrRequestInvalid
	}
	return s.level.Players[binding.PlayerID-1], nil
}

func (s *seats) content(_ context.Context, binding gateway.Binding) (roomwiring.RoomContent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		// Mod 先于服务 Init 启动，关卡还没加载时与 Seat 一样按“未就绪”拒绝，客户端重试。
		return roomwiring.RoomContent{}, domain.ErrStopping
	}
	body, err := s.body(binding)
	if err != nil {
		return roomwiring.RoomContent{}, err
	}
	return roomwiring.RoomContent{Data: s.data, SHA256: s.hash, Body: body}, nil
}

func (s *seats) seat(ctx context.Context, binding gateway.Binding) (domain.Ref, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rooms == nil {
		return domain.Ref{}, 0, domain.ErrStopping
	}
	body, err := s.body(binding)
	if err != nil {
		return domain.Ref{}, 0, err
	}
	if s.room.ID != "" {
		return s.room, body, nil
	}
	// Describe 取 owner 代际并随这次 Create 保留；失败时下一次输入重新开房，不复用未知结果的请求。
	instance, err := s.rooms.Describe(ctx)
	if err != nil {
		return domain.Ref{}, 0, fmt.Errorf("physicsroom owner (sid %d) unavailable: %w", ownerSID, err)
	}
	ref, err := s.rooms.Create(ctx, domain.Spec{Instance: instance, ID: "unity-" + token(), Bodies: s.level.Bodies, Motions: s.level.Motions, Joints: s.level.Joints, LifetimeSeconds: roomLifetime})
	if err != nil {
		return domain.Ref{}, 0, err
	}
	s.room = ref
	slog.Info("physics room created", "room", ref.ID, "owner", ownerSID, "level", s.level.Name, "revision", s.level.Revision, "lifetime_seconds", roomLifetime)
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
	if err := g.seats.load(); err != nil {
		return err
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
