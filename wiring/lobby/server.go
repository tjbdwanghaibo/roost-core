package lobby

import domain "github.com/tjbdwanghaibo/roost-core/service/lobby"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// ServiceName 是大厅服的 app.Service 名。
const ServiceName app.ServiceName = "lobby"

// eventTimeout 是处理一条 relay 报告（一次 Redis compare-and-set）的上限。
const eventTimeout = 5 * time.Second

// verifyTimeout 是答复一条验权请求（一次 Redis 读）的上限；relay 那边另有自己的期限（relay.admit_timeout）。
const verifyTimeout = 2 * time.Second

// Server 是大厅服的 app.Service：Serve 先订阅 relay 的房间事件与验权请求，再绑定监听、开始处理 HTTP，直到 ctx 结束；
// Shutdown 先退订（不再改目录），再停 HTTP（不接新请求，在 ctx 内等在途请求结束）。
type Server struct {
	lobby *Lobby
	nats  fnats.IClient

	mu       sync.Mutex
	sub      fnats.ISubscription
	verify   fnats.ISubscription
	http     *http.Server
	bound    string
	shutdown bool
}

// NewServer 返回大厅服的 app.Service。
func NewServer() *Server { return &Server{} }

// Name implements app.Service.
func (s *Server) Name() app.ServiceName { return ServiceName }

// Init 取 Mod 装配好的 *Lobby 与 NATS 客户端。
func (s *Server) Init(r *app.Registry) error {
	lobby, ok := app.Lookup[*Lobby](r, CapabilityName)
	if !ok || lobby == nil {
		return fmt.Errorf("lobby server: capability %q not found; add lobby.NewMod()", CapabilityName)
	}
	client, ok := app.Lookup[fnats.IClient](r, mods.ModNats)
	if !ok || client == nil {
		return fmt.Errorf("lobby server: capability %q not found", mods.ModNats)
	}
	s.lobby, s.nats = lobby, client
	return nil
}

// Serve implements app.Service。监听失败、订阅失败、HTTP 服务异常退出都是进程的失败。
func (s *Server) Serve(ctx context.Context) error {
	service := s.lobby.Service
	sub, err := s.nats.QueueSubscribe(domain.EventSubject(s.lobby.Subject), domain.EventQueue, func(msg *fnats.Msg) {
		eventCtx, cancel := context.WithTimeout(context.Background(), eventTimeout)
		defer cancel()
		if err := service.HandleRelayEventData(eventCtx, msg.Data); err != nil {
			slog.Warn("lobby: relay event not applied", "subject", msg.Subject, "err", err)
		}
	})
	if err != nil {
		return fmt.Errorf("lobby server: subscribe %s: %w", domain.EventSubject(s.lobby.Subject), err)
	}
	// relay 每条连接进房前的验权（方案 §4.3）：队列组里一个实例答复。答复发不出去只记日志，relay 等到期限按 lobby
	// 不可用拒绝那条连接。
	verify, err := s.nats.QueueSubscribe(domain.VerifySubject(s.lobby.Subject), domain.VerifyQueue, func(msg *fnats.Msg) {
		if msg.Reply == "" {
			return
		}
		verifyCtx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
		defer cancel()
		if err := s.nats.Publish(msg.Reply, service.HandleRelayVerifyData(verifyCtx, msg.Data)); err != nil {
			slog.Warn("lobby: relay verification reply not sent", "err", err)
		}
	})
	if err != nil {
		_ = sub.Unsubscribe()
		return fmt.Errorf("lobby server: subscribe %s: %w", domain.VerifySubject(s.lobby.Subject), err)
	}
	listener, err := net.Listen("tcp", s.lobby.Listen)
	if err != nil {
		_ = sub.Unsubscribe()
		_ = verify.Unsubscribe()
		return fmt.Errorf("lobby server: listen on lobby.listen %s: %w", s.lobby.Listen, err)
	}
	server := httpserver.NewServer(s.lobby.Listen, service.Handler(s.lobby.CORSOrigins))

	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		_ = sub.Unsubscribe()
		_ = verify.Unsubscribe()
		_ = listener.Close()
		return nil
	}
	s.sub, s.verify, s.http, s.bound = sub, verify, server, listener.Addr().String()
	s.mu.Unlock()
	slog.Info("lobby: serving", "addr", s.bound, "events", domain.EventSubject(s.lobby.Subject))

	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("lobby server: http: %w", err)
	}
}

// Addr 是实际监听的地址（lobby.listen 写端口 0 时由系统分配）；还没监听时为空。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bound
}

// Shutdown implements app.Service。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shutdown = true
	sub, verify, server := s.sub, s.verify, s.http
	s.sub, s.verify, s.http = nil, nil, nil
	s.mu.Unlock()
	var errs []error
	for _, subscription := range []fnats.ISubscription{sub, verify} {
		if subscription == nil {
			continue
		}
		if err := subscription.Unsubscribe(); err != nil {
			errs = append(errs, fmt.Errorf("lobby server: unsubscribe: %w", err))
		}
	}
	if server != nil {
		if err := server.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("lobby server: http shutdown: %w", err), server.Close())
		}
	}
	return errors.Join(errs...)
}

var _ app.Service = (*Server)(nil)
