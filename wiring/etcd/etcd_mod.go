package etcd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	etcddriver "github.com/tjbdwanghaibo/roost-core/infra/network/etcd/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"

	"github.com/spf13/viper"
)

// EtcdMod implements app.Mod for etcd connectivity. It parses configuration,
// asks core to assemble client / discovery / elections on one connection,
// publishes them as capabilities and forwards lifecycle calls; it holds no
// raw clientv3 handle (P3b).
type EtcdMod struct {
	registerService bool
	asm             *etcddriver.Assembly
	cfg             *fetcd.Config

	// service info for auto-registration
	serviceInfo *fetcd.ServiceInfo
}

// Option 只控制装配；连接、租约与注册循环仍由 etcd driver 持有。
type Option func(*EtcdMod)

// WithoutServiceRegistration 让接入 Mod 在监听/订阅启动后登记真实 incarnation。
// 一个 Discovery 只有一个注册所有者，不能与 EtcdMod 的自动登记同时使用。
func WithoutServiceRegistration() Option {
	return func(mod *EtcdMod) { mod.registerService = false }
}

func NewEtcdMod(options ...Option) *EtcdMod {
	mod := &EtcdMod{registerService: true}
	for _, option := range options {
		if option != nil {
			option(mod)
		}
	}
	return mod
}

func (m *EtcdMod) Name() app.ModName { return mods.ModEtcd }

// config 是 etcd.* 的声明（维护者决定 A4 ①）。
type config struct {
	app.ServiceIdentity
	Endpoints     []string `config:"etcd.endpoints" default:"localhost:2379" example:"127.0.0.1:2379" help:"etcd 地址（逗号分隔或 YAML 列表）"`
	Username      string   `config:"etcd.username" example:""`
	Password      string   `config:"etcd.password" example:""`
	ServicePrefix string   `config:"etcd.service_prefix" example:"/roost/services/" help:"服务注册的键前缀，不写取 etcd 包的缺省"`
	LeaseTTL      int64    `config:"etcd.lease_ttl" min:"0" example:"10" help:"注册租约秒数，0 取 etcd 包的缺省"`
	// AdvertiseAddr 是写进服务注册的地址；不写则注册不带地址。
	AdvertiseAddr    string        `config:"etcd.advertise_addr" example:"127.0.0.1:9000" help:"写进服务注册的地址（其他进程据此连接本服务）"`
	RegisterRetryMin time.Duration `config:"etcd.register_retry_min_interval" min:"0" help:"注册失败重试的最短间隔，0 取 etcd 包的缺省"`
	RegisterRetryMax time.Duration `config:"etcd.register_retry_max_interval" min:"0" help:"注册失败重试的最长间隔，0 取 etcd 包的缺省"`
}

// ConfigSchema 声明 etcd.*。
func (m *EtcdMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init 读 etcd.*。A4 ① 起删掉了旧 gate 服务的地址回退（gate.backend_advertise_addr / gate.backend_addr /
// game.advertise_addr）与 gate.* 元数据：仓内没有生成器写这些键，也没有任何代码读注册里的元数据。
func (m *EtcdMod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("etcd mod: %w", err)
	}
	m.cfg = fetcd.DefaultConfig(settings.Endpoints)
	m.cfg.Username = settings.Username
	m.cfg.Password = settings.Password
	if settings.ServicePrefix != "" {
		m.cfg.ServicePrefix = settings.ServicePrefix
	}
	if settings.LeaseTTL > 0 {
		m.cfg.LeaseTTL = settings.LeaseTTL
	}
	if settings.RegisterRetryMin > 0 {
		m.cfg.RegisterRetryMinInterval = settings.RegisterRetryMin
	}
	if settings.RegisterRetryMax > 0 {
		m.cfg.RegisterRetryMaxInterval = settings.RegisterRetryMax
	}
	m.serviceInfo = &fetcd.ServiceInfo{
		ServiceType: settings.ServerType,
		Sid:         settings.Sid,
		Addr:        settings.AdvertiseAddr,
	}
	if settings.AdvertiseAddr != "" {
		m.serviceInfo.Metadata = map[string]string{"addr": settings.AdvertiseAddr}
	}
	if !m.registerService {
		m.serviceInfo = nil
	}
	return nil
}

func (m *EtcdMod) Provide(r *app.Registry) error {
	asm, err := etcddriver.Assemble(m.cfg)
	if err != nil {
		return err
	}
	m.asm = asm
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return errors.New("etcd mod: health registry not found")
	}
	healthReg.Register("etcd", health.CheckerFunc(func(ctx context.Context) health.Result {
		if m.asm == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := m.asm.Ping(checkCtx); err != nil {
			return health.Result{Status: health.StatusFail, Message: "status failed", Err: err}
		}
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))

	return mods.RegisterAll(r,
		mods.Capability{Name: mods.ModEtcd, Value: fetcd.IEtcd(m.asm.Client)},
		mods.Capability{Name: mods.ModEtcdDiscov, Value: fetcd.IDiscovery(m.asm.Discovery)},
	)
}

// Start 连接并在 Discovery 里注册本进程（<service_prefix><server_type>/<sid>）。注册只做地址 / 元数据
// 发现，不是存活权威：“这个 sid 有没有进程在跑”以 App 单实例锁的 Live（app.ModSingleton）为准。
// Mod Start 在 App 拿到单实例锁之后，所以同一 sid 同一时刻只有一个进程注册。
func (m *EtcdMod) Start() error {
	if m == nil || m.asm == nil {
		return errors.New("etcd mod: not provided")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.asm.Start(ctx, m.serviceInfo); err != nil {
		return err
	}
	slog.Info("etcd mod: started", "endpoints", m.cfg.Endpoints)
	return nil
}

func (m *EtcdMod) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.StopWithContext(ctx); err != nil {
		slog.Warn("etcd mod: stop failed", "err", err)
	}
}

func (m *EtcdMod) StopWithContext(ctx context.Context) error {
	if m == nil || m.asm == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := m.asm.Close(ctx)
	slog.Info("etcd mod: stopped")
	return err
}
