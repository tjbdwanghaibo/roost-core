package syncbus

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/app"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	driver "github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"
	"log/slog"
	"time"

	"github.com/spf13/viper"
)

// defaultPrefix 是未配置 prefix 时本 Mod 用的主题前缀（沿用 room 时代的名字）。
const defaultPrefix = "roost.room"

// config 是 syncbus.* 的声明（维护者决定 A4 ①）。A4 ① 起只读 syncbus 段：旧的 room / sync 同名回退删除
// （线上未部署，不做旧格式兼容）；拼错的键由 App 启动时的未知键告警与 doctor 报出（RR-20260926-12 的初衷）。
// 时长与整数写 0 取驱动缺省。
type config struct {
	app.ServiceIdentity
	Transport      string        `config:"syncbus.transport" default:"nats" enum:"nats|jetstream|js" example:"jetstream" help:"服务间同步总线：nats 或 jetstream（持久投递）"`
	Prefix         string        `config:"syncbus.prefix" default:"roost.room" example:"roost.sync" help:"主题前缀"`
	Stream         string        `config:"syncbus.stream" help:"JetStream 流名。不写由 prefix 推出（roost.sync -> ROOST_SYNC，zz.sync -> ZZ_SYNC），共用一个 NATS 的部署 prefix 不同流就不同；只在要沿用已有的流与 consumer 游标时写"`
	Storage        string        `config:"syncbus.storage" enum:"file|memory" example:"file"`
	AckWait        time.Duration `config:"syncbus.ack_wait" min:"0"`
	MaxDeliver     int           `config:"syncbus.max_deliver" min:"0" help:"每消息总投递次数（含首次），0取默认5；到限终止，不因重订重置"`
	StreamMaxAge   time.Duration `config:"syncbus.stream_max_age" min:"0"`
	Duplicates     time.Duration `config:"syncbus.duplicates" min:"0"`
	Replicas       int           `config:"syncbus.replicas" min:"0" example:"1"`
	MaxBytes       int64         `config:"syncbus.max_bytes" min:"0"`
	SetupTimeout   time.Duration `config:"syncbus.setup_timeout" min:"0"`
	PublishTimeout time.Duration `config:"syncbus.publish_timeout" min:"0" example:"3s"`
}

func (c config) prefix() string {
	if c.Prefix != "" {
		return c.Prefix
	}
	return defaultPrefix
}

// SyncBusMod implements app.Mod, providing the service-to-service ISyncBus over NATS or JetStream.
// Depends on NATS Mod: 普通模式取 fnats.IClient，JetStream 模式取 ModNatsJetStream。
type SyncBusMod struct {
	bus       fsyncbus.ISyncBus
	localSid  int32
	prefix    string
	transport string
	jsCfg     driver.JetStreamSyncConfig
}

func NewSyncBusMod(localSid int32) *SyncBusMod {
	return &SyncBusMod{localSid: localSid}
}

func (m *SyncBusMod) Name() app.ModName { return mods.ModSyncBus }

// ConfigSchema 声明 syncbus.*。
func (m *SyncBusMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (m *SyncBusMod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("syncbus mod: %w", err)
	}
	if m.localSid == 0 {
		m.localSid = settings.Sid
	}
	m.prefix = settings.prefix()
	m.transport = settings.Transport
	m.jsCfg = driver.JetStreamSyncConfig{
		LocalSid:     m.localSid,
		Prefix:       m.prefix,
		Stream:       jetStreamStream(settings.Stream, m.prefix),
		Storage:      fnats.JetStreamStorage(settings.Storage),
		AckWait:      settings.AckWait,
		MaxDeliver:   settings.MaxDeliver,
		StreamMaxAge: settings.StreamMaxAge,
		Duplicates:   settings.Duplicates,
		Replicas:     settings.Replicas,
		MaxBytes:     settings.MaxBytes,
		SetupTimeout: settings.SetupTimeout,
		PublishTime:  settings.PublishTimeout,
	}
	return nil
}

// JetStreamStreamFromConfig 返回本 Mod 按 cfg 启动 JetStream 时实际使用的流名，规则与 Init 相同：
// 未写 prefix 时的缺省 roost.room、roost.room → ROOST_SYNC 的兼容映射、显式 stream 优先。生成工程的测试用它推期望流名（RR-20260927-35）：之前测试自己按
// driver.JetStreamSyncStream(prefix) 推，漏了兼容映射，按迁移说明保留 prefix: roost.room 的工程
// 期望 ROOST_ROOM、实际 ROOST_SYNC，测试误报失败。只读 cfg，不校验 transport。
func JetStreamStreamFromConfig(cfg *viper.Viper) string {
	var settings config
	_ = app.LoadConfig(cfg, &settings) // 只推流名；写错的值由 Init 报出
	return jetStreamStream(settings.Stream, settings.prefix())
}

// jetStreamStream 决定 JetStream 流名（RR-20260926-56）：显式 stream 优先；否则由 prefix 派生，
// 不同 prefix 的部署共用一个 NATS 时各有各的流。本 Mod 的缺省 prefix roost.room 与生成配置的
// roost.sync 在修复前都落在 ROOST_SYNC 上，仍映射到 ROOST_SYNC，已部署的流与 durable 游标不变。
func jetStreamStream(configured, prefix string) string {
	if configured != "" {
		return configured
	}
	if prefix == defaultPrefix {
		return driver.JetStreamSyncStream("")
	}
	return driver.JetStreamSyncStream(prefix)
}

func (m *SyncBusMod) Provide(r *app.Registry) error {
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return fmt.Errorf("syncbus mod: capability %q not found", mods.ModHealth)
	}
	if m.useJetStream() {
		js, ok := app.Lookup[fnats.IJetStream](r, mods.ModNatsJetStream)
		if !ok || js == nil {
			return fmt.Errorf("syncbus mod: required capability %q not found", mods.ModNatsJetStream)
		}
		bus, err := driver.NewJetStreamSyncBus(fctx.BaseContext(), js, m.jsCfg)
		if err != nil {
			return err
		}
		m.bus = bus
		m.registerHealth(healthReg, "jetstream")
		return r.Register(mods.ModSyncBus, m.bus)
	}
	client, ok := app.Lookup[fnats.IClient](r, mods.ModNats)
	if !ok {
		return fmt.Errorf("syncbus mod: required capability %q not found", mods.ModNats)
	}
	m.bus = driver.NewNatsSyncBus(client, m.localSid, m.prefix)
	m.registerHealth(healthReg, "nats")
	return r.Register(mods.ModSyncBus, m.bus)
}

func (m *SyncBusMod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModNats}
}

func (m *SyncBusMod) Start() error {
	if m.useJetStream() {
		// 实际流名写进启动日志：流名决定与谁共享消息与游标（RR-20260926-56）。
		slog.Info("syncbus mod: started", "transport", "jetstream", "prefix", m.prefix, "stream", m.jsCfg.Stream)
		return nil
	}
	slog.Info("syncbus mod: started", "transport", "nats", "prefix", m.prefix)
	return nil
}

func (m *SyncBusMod) Stop() {
	if err := m.StopWithContext(fctx.BaseContext()); err != nil {
		slog.Warn("syncbus mod: stop interrupted", "err", err)
	}
}

func (m *SyncBusMod) StopWithContext(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	if stopper, ok := m.bus.(interface{ StopWithContext(context.Context) error }); ok {
		if err := stopper.StopWithContext(ctx); err != nil {
			return err
		}
	} else if stopper, ok := m.bus.(interface{ Stop() }); ok {
		stopper.Stop()
	}
	m.bus = nil
	slog.Info("syncbus mod: stopped")
	return nil
}

func (m *SyncBusMod) useJetStream() bool {
	return m != nil && (m.transport == "jetstream" || m.transport == "js")
}

func (m *SyncBusMod) registerHealth(reg *health.Registry, transport string) {
	if reg == nil {
		return
	}
	reg.Register("sync", health.CheckerFunc(func(context.Context) health.Result {
		if m == nil || m.bus == nil {
			return health.Result{Status: health.StatusFail, Message: "bus not initialized"}
		}
		return health.Result{Status: health.StatusOK, Message: transport}
	}))
}
