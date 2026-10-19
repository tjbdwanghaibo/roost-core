// Package web 是业务 HTTP 路由（roost webroute 生成的 RegisterRoutes）的监听 Mod。
//
// 生命周期与所有权：
//   - Init 读 web.* 声明；Provide 把本 Mod 作为 webroute.Registerer 登记为 mods.ModWeb，并挂两个 lifecycle hook；
//   - Start 只 bind（端口被占用时启动失败，而不是进程在没有路由的情况下继续跑），不处理请求；
//   - 服务在 Service.Init 里经 RegisterRoutes 登记路由；service.started 时冻结路由表、开始 Serve——请求处理与路由
//     登记不会并发（chi 的路由树没有锁），Init 之后再登记一律拒绝；
//   - service.stopping 时 http.Server.Shutdown：不再接新请求、在停机预算内等在途请求结束，Service.Shutdown 之后
//     不会再有业务请求进来；Stop 收尾仍未关闭的监听。
//
// 鉴权：生成的 JSON 路由拿不到请求头，本 Mod 也不做鉴权。缺省只绑回环地址；生产环境绑非回环地址必须显式写
// web.allow_public_addr: true（端点放在鉴权代理之后时），与 ops 的规则相同。
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
	"github.com/tjbdwanghaibo/roost-core/infra/network/webroute"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// config 是 web.* 的声明。
type config struct {
	Enabled         bool   `config:"web.enabled" default:"true" example:"true" help:"监听业务 HTTP 路由（roost add webroute）；false 时路由照常登记但不监听"`
	Addr            string `config:"web.addr" default:"127.0.0.1:8080" example:"127.0.0.1:8080" help:"业务 HTTP 路由的监听地址；生产环境绑非回环地址要同时写 web.allow_public_addr: true"`
	AllowPublicAddr bool   `config:"web.allow_public_addr" help:"生产环境允许 web.addr 不是回环地址（路由放在鉴权代理之后时才写 true；本 Mod 不做鉴权）"`
}

// ValidateConfig：生产环境的业务 HTTP 端点不默认暴露到公网（路由本身没有鉴权）。
func (c *config) ValidateConfig(production bool) error {
	if production && c.Enabled && !isLoopbackListenAddr(c.Addr) && !c.AllowPublicAddr {
		return fmt.Errorf("config: production web.addr %q is not loopback; bind 127.0.0.1 or set web.allow_public_addr=true after putting the routes behind an authenticated proxy", c.Addr)
	}
	return nil
}

// isLoopbackListenAddr 与 ops 的同名规则相同：只写端口、空或通配主机表示所有网卡。
func isLoopbackListenAddr(addr string) bool {
	host := addr
	if parsed, _, err := net.SplitHostPort(addr); err == nil {
		host = parsed
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Mod 持有路由表与监听。mu 保护下面的状态；server 只在 Start 之后、listener 非 nil 时有意义。
type Mod struct {
	mu        sync.Mutex
	cfg       config
	engine    *httpserver.Engine
	registrar *webroute.Registrar
	serving   bool // service.started 之后为 true：路由表冻结，之后的 Register 拒绝
	listener  net.Listener
	server    *http.Server
	boundAddr string
}

func NewMod() *Mod {
	engine := httpserver.NewEngine()
	return &Mod{engine: engine, registrar: webroute.NewRegistrar(engine)}
}

func (m *Mod) Name() app.ModName              { return mods.ModWeb }
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (m *Mod) Init(cfg *viper.Viper) error {
	var c config
	if err := app.LoadConfig(cfg, &c); err != nil {
		return fmt.Errorf("web: %w", err)
	}
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
	return nil
}

func (m *Mod) Provide(r *app.Registry) error {
	if r == nil {
		return errors.New("web: app registry is nil")
	}
	life, ok := app.Lookup[*lifecycle.Registry](r, mods.ModLifecycle)
	if !ok || life == nil {
		return fmt.Errorf("web: capability %q not found", mods.ModLifecycle)
	}
	for _, hook := range []lifecycle.Hook{
		{Name: "web.serve.service_started", Phase: lifecycle.PhaseServiceStarted, Handler: func(context.Context, lifecycle.Event) error { return m.serve() }},
		{Name: "web.shutdown.service_stopping", Phase: lifecycle.PhaseServiceStopping, Handler: func(ctx context.Context, _ lifecycle.Event) error { return m.shutdown(ctx) }},
	} {
		if err := life.Register(hook); err != nil {
			return err
		}
	}
	return r.Register(mods.ModWeb, m)
}

// Register 实现 webroute.Registerer：只在 Service.Init 期间（service.started 之前）接受登记。
func (m *Mod) Register(method, path string, handler http.HandlerFunc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serving {
		return fmt.Errorf("web: route %s %s registered after the service started; register routes in Service.Init", method, path)
	}
	return m.registrar.Register(method, path, handler)
}

// Start 同步 bind：返回 nil 表示端口已经拿到；请求要等 service.started 才处理（连接在 backlog 里等）。
func (m *Mod) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Enabled {
		slog.Info("web: disabled (web.enabled=false); routes are registered but not served")
		return nil
	}
	if m.listener != nil {
		return errors.New("web: already started")
	}
	listener, err := net.Listen("tcp", m.cfg.Addr)
	if err != nil {
		return fmt.Errorf("web: listen on web.addr %s: %w", m.cfg.Addr, err)
	}
	m.listener = listener
	m.boundAddr = listener.Addr().String()
	m.server = httpserver.NewServer(m.cfg.Addr, m.engine)
	return nil
}

// Addr 是实际监听的地址（web.addr 写端口 0 时由系统分配）；没有监听时为空。
func (m *Mod) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.boundAddr
}

func (m *Mod) serve() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serving {
		return nil
	}
	m.serving = true
	if m.listener == nil {
		return nil
	}
	server, listener, addr := m.server, m.listener, m.boundAddr
	go func() {
		// Serve 在 Shutdown / Close 之后返回 ErrServerClosed，并关闭 listener。
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web: http server failed", "addr", addr, "err", err)
		}
	}()
	slog.Info("web: serving", "addr", addr)
	return nil
}

// shutdown 在 service.stopping 时停止接新请求并等在途请求在 ctx 内结束；ctx 到期时强制关闭连接并返回错误。
func (m *Mod) shutdown(ctx context.Context) error {
	m.mu.Lock()
	server, listener, serving := m.server, m.listener, m.serving
	m.server, m.listener = nil, nil
	m.mu.Unlock()
	if server == nil {
		return nil
	}
	if !serving {
		// 只 bind 过、从没 Serve：Shutdown 不认识这个 listener，直接关掉。
		return listener.Close()
	}
	if err := server.Shutdown(ctx); err != nil {
		closeErr := server.Close()
		return errors.Join(fmt.Errorf("web: shutdown: %w", err), closeErr)
	}
	return nil
}

// Stop 收尾 service.stopping 没走到的情况（启动失败后的逆序停止）。
func (m *Mod) Stop() {
	m.mu.Lock()
	server, listener, serving := m.server, m.listener, m.serving
	m.server, m.listener = nil, nil
	m.mu.Unlock()
	if server == nil {
		return
	}
	if !serving {
		_ = listener.Close()
		return
	}
	_ = server.Close()
}

// RegisterRoutes 是服务 Init 里的一行接线：按 mods.ModWeb 取本进程的 web Mod，按顺序登记生成的路由模块
// （例如 &web.Service{}，webroute 生成它的 RegisterRoutes）。服务没装 web Mod 时报错并给出命令。
func RegisterRoutes(registry *app.Registry, modules ...webroute.Module) error {
	if registry == nil {
		return errors.New("web: app registry is nil")
	}
	registerer, ok := app.Lookup[webroute.Registerer](registry, mods.ModWeb)
	if !ok || registerer == nil {
		return fmt.Errorf("web: capability %q not found; add the web mod to this service (roost add mod web -service <service>)", mods.ModWeb)
	}
	return webroute.RegisterModules(registerer, modules...)
}

var (
	_ app.Mod             = (*Mod)(nil)
	_ webroute.Registerer = (*Mod)(nil)
)
