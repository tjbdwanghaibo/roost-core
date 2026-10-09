package ops

import (
	"context"
	"errors"
	"fmt"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	runtime "github.com/tjbdwanghaibo/roost-core/infra/observe/ops"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"net"
	"strings"
	"time"
)

// OpsMod 只读取配置、连接观测能力并把启停交给唯一 HTTP runtime。
type OpsMod struct{ *runtime.Server }

func NewOpsMod() *OpsMod            { return &OpsMod{Server: runtime.New()} }
func (m *OpsMod) Name() app.ModName { return mods.ModOps }

// config 是 ops.* 的声明（维护者决定 A4 ①）。
type config struct {
	app.ServiceIdentity
	Enabled         bool          `config:"ops.enabled" example:"true" help:"打开 ops 端点（/healthz、/readyz、/metrics、/statsz、/admin）"`
	Addr            string        `config:"ops.addr" default:"127.0.0.1:9100" example:"127.0.0.1:9100" help:"ops 端点监听地址；生产环境绑公网要同时写 ops.allow_public_addr: true"`
	AllowPublicAddr bool          `config:"ops.allow_public_addr" help:"生产环境允许 ops.addr 不是回环地址（端点放在鉴权代理之后时才写 true）"`
	AdminEnabled    bool          `config:"ops.admin_enabled" example:"false" help:"打开 /admin 命令；打开时必须写 ops.admin_token"`
	AdminToken      string        `config:"ops.admin_token" secret:"optional" example:"" help:"/admin 的令牌；生产禁止 dev-，开发使用 dev- 时须写 ops.allow_dev_token: true"`
	AllowDevToken   bool          `config:"ops.allow_dev_token" example:"false"`
	AdminTimeout    time.Duration `config:"ops.admin_timeout" default:"10s" min:"1ns" help:"/admin/execute 交给命令的期限"`
}

// ValidateConfig：打开 admin 必须有令牌、dev- 令牌要显式允许；生产环境 ops 端点不绑公网（以前在 app 的启动校验里）。
//
// ops 端点带着不鉴权的 /metrics，打开 admin 时还能执行全部已登记的 admin 命令。回环地址只是缺省值，所以绑到所有网卡
// 必须是声明过的决定（端点放在鉴权代理之后时写 ops.allow_public_addr: true），而不是忘了改的缺省。
func (c *config) ValidateConfig(production bool) error {
	var errs []error
	if c.AdminEnabled {
		if c.AdminToken == "" {
			errs = append(errs, errors.New("config: ops.admin_enabled requires admin_token: ops.admin_token is empty"))
		}
		if strings.HasPrefix(c.AdminToken, "dev-") && (production || !c.AllowDevToken) {
			errs = append(errs, errors.New("config: dev ops.admin_token is forbidden in production and otherwise requires ops.allow_dev_token=true"))
		}
	}
	if production && c.Enabled && !isLoopbackListenAddr(c.Addr) && !c.AllowPublicAddr {
		errs = append(errs, fmt.Errorf(
			"config: production ops.addr %q is not loopback; bind 127.0.0.1 or set ops.allow_public_addr=true after putting the endpoint behind an authenticated proxy", c.Addr))
	}
	return errors.Join(errs...)
}

// isLoopbackListenAddr reports whether a listen address is reachable only from
// the host. A bare port or an empty/wildcard host means "every interface".
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

func (m *OpsMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }
func (m *OpsMod) Init(cfg *viper.Viper) error {
	var c config
	if err := app.LoadConfig(cfg, &c); err != nil {
		return fmt.Errorf("ops: %w", err)
	}
	return m.Configure(runtime.Config{Enabled: c.Enabled, Addr: c.Addr, AdminEnabled: c.AdminEnabled, AdminToken: c.AdminToken, AdminTimeout: c.AdminTimeout, Service: c.ServerType, SID: c.Sid})
}

type statsCollector interface{ CollectStats() any }

func (m *OpsMod) Provide(r *app.Registry) error {
	if r == nil {
		return fmt.Errorf("ops: app registry is nil")
	}
	h, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || h == nil {
		return fmt.Errorf("ops: capability %q not found", mods.ModHealth)
	}
	metric, ok := app.Lookup[*metrics.Registry](r, mods.ModMetrics)
	if !ok || metric == nil {
		return fmt.Errorf("ops: capability %q not found", mods.ModMetrics)
	}
	commands, ok := app.Lookup[*admin.Registry](r, mods.ModAdmin)
	if !ok || commands == nil {
		return fmt.Errorf("ops: capability %q not found", mods.ModAdmin)
	}
	life, ok := app.Lookup[*lifecycle.Registry](r, mods.ModLifecycle)
	if !ok || life == nil {
		return fmt.Errorf("ops: capability %q not found", mods.ModLifecycle)
	}
	if err := m.Connect(runtime.Dependencies{Health: h, Metrics: metric, Commands: commands, Stats: func() (any, error) {
		collector, ok := app.Lookup[statsCollector](r, mods.ModStatsLog)
		if !ok || collector == nil {
			return nil, runtime.ErrStatsNotFound
		}
		return collector.CollectStats(), nil
	}}); err != nil {
		return err
	}
	for _, hook := range []lifecycle.Hook{{Name: "ops.ready.service_started", Phase: lifecycle.PhaseServiceStarted, Handler: func(context.Context, lifecycle.Event) error { m.SetReady(true, "service started"); return nil }}, {Name: "ops.ready.service_stopping", Phase: lifecycle.PhaseServiceStopping, Handler: func(context.Context, lifecycle.Event) error { m.SetReady(false, "service stopping"); return nil }}} {
		if err := life.Register(hook); err != nil {
			return err
		}
	}
	return r.Register(mods.ModOps, m)
}

var _ app.Mod = (*OpsMod)(nil)
