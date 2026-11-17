package relay

import domain "github.com/tjbdwanghaibo/roost-core/service/relay"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/wsconn"
)

// runtime 是 Mod 装配好、交给 Server 的东西。
type runtime struct {
	server         *domain.Server
	nats           fnats.RawClient
	listen         string
	wsListen       string        // 空表示不开 WebSocket
	ws             wsconn.Config // wsListen 非空时有效
	commandSubject string
	advertiser     *advertiser
}

// Server 是 relay 进程的 app.Service。
//
// Init：开 TCP 监听（配了 relay.ws_listen 时再开一个 WebSocket 监听，两种连接进同一批房间）、订阅 lobby 的命令主题
// （失败就起不来）。Serve：接受连接、每个房间定时心跳、etcd 广播，直到 ctx 结束。
// Shutdown：先停命令订阅，再关监听与全部房间（每条连接收到 Closed{shutdown}），最后撤掉 etcd 广播。
type Server struct {
	rt        *runtime
	listeners []net.Listener
	commands  fnats.DrainSubscription
	serving   sync.WaitGroup
}

// NewServer 返回 relay 的 app.Service；同一进程要有 NewMod()。
func NewServer() *Server { return &Server{} }

// Name 实现 app.Service。
func (s *Server) Name() app.ServiceName { return ServiceName }

// Relay 返回 relay 本体（Init 之后可用）。
func (s *Server) Relay() *domain.Server {
	if s.rt == nil {
		return nil
	}
	return s.rt.server
}

// Addr 是 TCP 监听的实际地址（Init 之后可用；listen 写 :0 时由它取端口）。
func (s *Server) Addr() net.Addr {
	if len(s.listeners) == 0 {
		return nil
	}
	return s.listeners[0].Addr()
}

// WSAddr 是 WebSocket 监听的实际地址；没开 WebSocket 时为 nil。
func (s *Server) WSAddr() net.Addr {
	if len(s.listeners) < 2 {
		return nil
	}
	return s.listeners[1].Addr()
}

// Init 取 Mod 的装配结果，开监听、订阅命令。
func (s *Server) Init(r *app.Registry) error {
	rt, ok := app.Lookup[*runtime](r, runtimeName)
	if !ok || rt == nil {
		return fmt.Errorf("relay server: capability %q not found; add relay.NewMod()", runtimeName)
	}
	s.rt = rt
	tcp, err := net.Listen("tcp", rt.listen)
	if err != nil {
		return fmt.Errorf("relay server: listen %s: %w", rt.listen, err)
	}
	s.listeners = append(s.listeners, tcp)
	if rt.wsListen != "" {
		ws, err := wsconn.Listen(rt.wsListen, rt.ws)
		if err != nil {
			s.closeListeners()
			return fmt.Errorf("relay server: websocket listen %s: %w", rt.wsListen, err)
		}
		s.listeners = append(s.listeners, ws)
	}
	s.commands, err = rt.nats.SubscribeBounded(rt.commandSubject, fnats.PendingLimits{Messages: 4096, Bytes: 4 << 20,
		OnError: func(err error) {
			slog.Warn("relay: lobby command subscription", "subject", rt.commandSubject, "err", err)
		}},
		func(msg *fnats.Msg) { handleCommand(rt.server, msg.Data) })
	if err != nil {
		s.closeListeners()
		return fmt.Errorf("relay server: subscribe %s: %w", rt.commandSubject, err)
	}
	return nil
}

// Serve 接受连接、发心跳、维护 etcd 广播，直到 ctx 结束或某个监听出错。
func (s *Server) Serve(ctx context.Context) error {
	if s.rt == nil {
		return errors.New("relay server: not initialised")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failed := make(chan error, len(s.listeners))
	for _, listener := range s.listeners {
		s.serving.Add(1)
		go func() {
			defer s.serving.Done()
			if err := s.rt.server.Serve(listener); err != nil {
				failed <- fmt.Errorf("relay server: serve %s: %w", listener.Addr(), err)
			}
		}()
	}
	s.serving.Add(2)
	go func() { defer s.serving.Done(); _ = s.rt.server.Run(ctx) }()
	go func() { defer s.serving.Done(); s.rt.advertiser.run(ctx) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-failed:
		return err
	}
}

// Shutdown 停命令订阅、关房断连接、撤 etcd 广播。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.rt == nil {
		return nil
	}
	var errs []error
	if s.commands != nil {
		if err := s.commands.DrainContext(ctx); err != nil {
			errs = append(errs, fmt.Errorf("relay server: drain lobby commands: %w", err))
		}
	}
	if err := s.rt.server.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	s.closeListeners() // Init 之后 Serve 没跑起来时，监听还在这里
	done := make(chan struct{})
	go func() { s.serving.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	if err := s.rt.advertiser.withdraw(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Server) closeListeners() {
	for _, listener := range s.listeners {
		_ = listener.Close()
	}
}

// command 是 lobby 发来的命令（service/lobby.RelayCommand）。
type command struct {
	Type    string `json:"type"`
	Room    string `json:"room"`
	Account string `json:"account"`
}

// commandLeave 与 service/lobby.CommandLeave 同值。
const commandLeave = "leave"

// handleCommand 处理一条 lobby 命令。解不开、类型不认识的命令记日志后丢弃：命令是尽力而为的，房间状态以 relay 为准。
func handleCommand(server *domain.Server, data []byte) {
	var cmd command
	if err := json.Unmarshal(data, &cmd); err != nil {
		slog.Warn("relay: unreadable lobby command", "err", err)
		return
	}
	switch cmd.Type {
	case commandLeave:
		server.Leave(cmd.Room, cmd.Account)
	default:
		slog.Warn("relay: unknown lobby command", "type", cmd.Type)
	}
}

// natsReporter 把房间报告编码成 JSON 发到 <subject>.events。发不出去只记日志：报告是尽力而为的（service/relay 的 report.go）。
type natsReporter struct {
	client  fnats.RawClient
	subject string
}

func (r natsReporter) Report(report domain.Report) {
	data, err := json.Marshal(report)
	if err == nil {
		err = r.client.PublishOnce(r.subject, data)
	}
	if err != nil {
		slog.Warn("relay: report to lobby failed", "subject", r.subject, "room", report.Room, "type", report.Type, "err", err)
	}
}

// verifyRequest / verifyReply 是向 lobby 验权的请求与答复（service/lobby.RelayVerifyRequest / RelayVerifyReply 的 JSON）。
type verifyRequest struct {
	Room    string `json:"room"`
	Account string `json:"account"`
	Relay   string `json:"relay"`
	Create  bool   `json:"create,omitempty"`
}

type verifyReply struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// verifyUnavailable 与 service/lobby.VerifyUnavailable 同值：lobby 自己出了错，按不可用处理（不是拒绝）。
const verifyUnavailable = "unavailable"

// natsAdmission 经 NATS request 向 lobby 验权（service/relay.Admission）：lobby 答 ok 放行；答别的原因是拒绝
// （ErrAdmissionDenied）；请求失败、超时、没有 lobby 应答、答复解不开、lobby 答 unavailable 都按不可用处理。
// 两种结果 relay 都拒绝这条连接（失败即关闭）。
type natsAdmission struct {
	client  fnats.RawClient
	subject string
}

func (a natsAdmission) Admit(ctx context.Context, request domain.AdmitRequest) error {
	data, err := json.Marshal(verifyRequest{Room: request.Room, Account: request.Account, Relay: request.Relay, Create: request.Create})
	if err != nil {
		return err
	}
	answer, err := a.client.RequestContext(ctx, a.subject, data)
	if err != nil {
		return fmt.Errorf("relay: ask the lobby (%s): %w", a.subject, err)
	}
	var reply verifyReply
	if err := json.Unmarshal(answer, &reply); err != nil {
		return fmt.Errorf("relay: unreadable lobby verification reply: %w", err)
	}
	switch {
	case reply.OK:
		return nil
	case reply.Reason == verifyUnavailable || reply.Reason == "":
		return fmt.Errorf("relay: the lobby could not verify the connection (%q)", reply.Reason)
	}
	return fmt.Errorf("%w: %s", domain.ErrAdmissionDenied, reply.Reason)
}

// Advertisement 是 relay 实例在 etcd 里的广播（service/lobby.Relay 的 JSON）。
type Advertisement struct {
	Instance string `json:"instance"`
	TCPAddr  string `json:"tcp_addr,omitempty"`
	WSAddr   string `json:"ws_addr,omitempty"`
	Rooms    int    `json:"rooms"`
}

// advertiseInterval 是检查房间数变化的间隔：房间数变了才重写键，合并成每秒最多一次（与 lobby 的约定）。
const advertiseInterval = time.Second

// advertiser 维护 etcd 键 <prefix><instance>：挂在自己的租约上，房间数变了就重写；租约丢了（etcd 断开太久）重新申请。
type advertiser struct {
	client fetcd.IEtcd
	key    string
	ttl    time.Duration
	base   Advertisement
	stats  interface{ Stats() domain.Stats }

	mu    sync.Mutex
	lease int64
}

func newAdvertiser(client fetcd.IEtcd, prefix string, ttl time.Duration, base Advertisement, stats interface{ Stats() domain.Stats }) *advertiser {
	return &advertiser{client: client, key: prefix + base.Instance, ttl: ttl, base: base, stats: stats}
}

func (a *advertiser) run(ctx context.Context) {
	ticker := time.NewTicker(advertiseInterval)
	defer ticker.Stop()
	var lost <-chan struct{}
	written := ""
	for {
		if lost == nil {
			var err error
			if lost, err = a.grant(ctx); err != nil {
				slog.Warn("relay: etcd advertisement lease", "key", a.key, "err", err)
			}
			written = ""
		}
		if lost != nil {
			value := a.value()
			if value != written {
				if err := a.client.PutWithLease(ctx, a.key, value, a.currentLease()); err != nil {
					slog.Warn("relay: etcd advertisement", "key", a.key, "err", err)
				} else {
					written = value
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-lost:
			slog.Warn("relay: etcd advertisement lease lost; re-registering", "key", a.key)
			lost = nil
		case <-ticker.C:
		}
	}
}

// grant 申请租约并开始续约，返回租约丢失的信号。
func (a *advertiser) grant(ctx context.Context) (<-chan struct{}, error) {
	seconds := max(int64(a.ttl/time.Second), 1)
	lease, err := a.client.Grant(ctx, seconds)
	if err != nil {
		return nil, err
	}
	lost, err := a.client.KeepAlive(ctx, lease)
	if err != nil {
		_ = a.client.Revoke(context.WithoutCancel(ctx), lease)
		return nil, err
	}
	a.mu.Lock()
	a.lease = lease
	a.mu.Unlock()
	return lost, nil
}

func (a *advertiser) currentLease() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lease
}

func (a *advertiser) value() string {
	advert := a.base
	advert.Rooms = a.stats.Stats().Rooms
	data, _ := json.Marshal(advert)
	return string(data)
}

// withdraw 撤销租约，键随之删除：停机后 lobby 立刻不再把新房间分到这里。租约已不在（过期）也算撤掉了。
func (a *advertiser) withdraw(ctx context.Context) error {
	a.mu.Lock()
	lease := a.lease
	a.lease = 0
	a.mu.Unlock()
	if lease == 0 {
		return nil
	}
	if err := a.client.Revoke(ctx, lease); err != nil {
		slog.Warn("relay: revoke etcd advertisement", "key", a.key, "err", err)
	}
	return nil
}
