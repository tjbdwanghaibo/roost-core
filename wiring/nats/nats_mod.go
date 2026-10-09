package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"

	"github.com/spf13/viper"
)

// NatsMod implements app.Mod for NATS connectivity. Core assembles the
// connection, JetStream and RPC client; the Mod parses configuration, builds
// the bus from registry configuration on top of them, publishes the
// capabilities and forwards lifecycle calls (P3b).
type NatsMod struct {
	// stopSerial 串行化 StopWithContext（后到者在自己的 ctx 内等第一个做完）；mu 保护健康检查读的 asm，
	// 健康检查可能与停止并发。bus 只在 Provide / Start / Stop 里读写，由 App 串行调用（RR-20261006-10）。
	stopSerial operation.Serial
	mu         sync.Mutex
	asm        *natsdriver.Assembly
	bus        *bus.Bus
	codec      bus.Codec
	cfg        *fnats.Config
	extra      natsdriver.ClientOptions
	settings   config
}

// config 是 nats.* 的声明（维护者决定 A4 ①）。nats.rpc.* 只在 transport 为 jetstream / js 时生效，写了就检查。
type config struct {
	app.ServiceIdentity
	URL                     string `config:"nats.url" default:"nats://localhost:4222" example:"nats://127.0.0.1:4222"`
	Prefix                  string `config:"nats.prefix" default:"roost" example:"roost" help:"bus 主题前缀"`
	WorkerNum               int    `config:"nats.worker_num" default:"8" min:"1" example:"8"`
	IgnoreDiscoveredServers bool   `config:"nats.ignore_discovered_servers" help:"只连配置的地址，不跟随集群 gossip 发现的节点（代理、NAT、故障注入时用）"`
	Reliable                struct {
		Enabled  bool          `config:"enabled" example:"false" help:"可靠总线（需要 Redis Mod）"`
		Prefix   string        `config:"prefix"`
		InboxTTL time.Duration `config:"inbox_ttl" min:"1ns"`
		DLQTTL   time.Duration `config:"dlq_ttl" min:"1ns"`
	} `config:"nats.reliable"`
	RPC struct {
		Transport      string        `config:"transport" default:"core" enum:"core|nats|jetstream|js" help:"RPC 传输：core / nats 是 NATS request-reply，jetstream / js 走 JetStream"`
		RequestStream  string        `config:"request_stream"`
		ResponseStream string        `config:"response_stream"`
		AckWait        time.Duration `config:"ack_wait" min:"1ns"`
		MaxDeliver     int           `config:"max_deliver" min:"1"`
		RequestTTL     time.Duration `config:"request_ttl" min:"1ns"`
		CallTimeout    time.Duration `config:"call_timeout" min:"1ns"`
		StreamMaxAge   time.Duration `config:"stream_max_age" min:"1ns"`
		Duplicates     time.Duration `config:"duplicates" min:"1ns"`
		Replicas       int           `config:"replicas" min:"0"`
		MaxBytes       int64         `config:"max_bytes" min:"0"`
		SetupTimeout   time.Duration `config:"setup_timeout" min:"1ns"`
	} `config:"nats.rpc"`
}

// jetStreamRPC 返回 JetStream RPC 的配置；transport 不是 jetstream / js 时第二个返回值为 false。
func (c config) jetStreamRPC() (bus.JetStreamRPCConfig, bool) {
	if c.RPC.Transport != "jetstream" && c.RPC.Transport != "js" {
		return bus.JetStreamRPCConfig{}, false
	}
	return bus.JetStreamRPCConfig{
		RequestStream: c.RPC.RequestStream, ResponseStream: c.RPC.ResponseStream,
		AckWait: c.RPC.AckWait, MaxDeliver: c.RPC.MaxDeliver, RequestTTL: c.RPC.RequestTTL, CallTimeout: c.RPC.CallTimeout,
		StreamMaxAge: c.RPC.StreamMaxAge, Duplicates: c.RPC.Duplicates, Replicas: c.RPC.Replicas, MaxBytes: c.RPC.MaxBytes,
		SetupTimeout: c.RPC.SetupTimeout,
	}, true
}

// NewNatsMod creates a NatsMod with an optional codec.
// If codec is nil, a JSON codec will be used by default.
func NewNatsMod(codec bus.Codec) *NatsMod {
	return &NatsMod{codec: codec}
}

func (m *NatsMod) Name() app.ModName { return mods.ModNats }

// OptionalDependsOn makes reliable-bus integration independent of the order
// in which applications list Mods. Redis remains optional for plain NATS.
func (m *NatsMod) OptionalDependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// ConfigSchema 声明 nats.*。
func (m *NatsMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init 按声明读完 nats.* 的全部键：写错的值在建连接与 bus 之前一次报出。
func (m *NatsMod) Init(cfg *viper.Viper) error {
	if err := app.LoadConfig(cfg, &m.settings); err != nil {
		return fmt.Errorf("nats mod: %w", err)
	}
	m.cfg = fnats.DefaultConfig(m.settings.URL)
	m.extra = natsdriver.ClientOptions{IgnoreDiscoveredServers: m.settings.IgnoreDiscoveredServers}
	return nil
}

func (m *NatsMod) Provide(r *app.Registry) error {
	asm, err := natsdriver.Assemble(m.cfg, m.extra)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.asm = asm
	m.mu.Unlock()
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return errors.New("nats mod: health registry not found")
	}
	adminReg, ok := app.Lookup[*admin.Registry](r, mods.ModAdmin)
	if !ok || adminReg == nil {
		return errors.New("nats mod: admin registry not found")
	}
	healthReg.Register("nats", health.CheckerFunc(func(context.Context) health.Result {
		m.mu.Lock()
		asm := m.asm
		m.mu.Unlock()
		if asm == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		if !asm.Connected() {
			return health.Result{Status: health.StatusFail, Message: "not connected"}
		}
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))
	settings := m.settings
	rpcCfg, rpcEnabled := settings.jetStreamRPC()
	reliable := bus.ReliableConfig{
		Enabled:  true,
		Prefix:   settings.Reliable.Prefix,
		InboxTTL: settings.Reliable.InboxTTL,
		DLQTTL:   settings.Reliable.DLQTTL,
	}
	reliableEnabled := settings.Reliable.Enabled

	m.bus = bus.New(m.asm.Client, m.asm.RPC, m.codec, bus.Config{
		Sid:       settings.Sid,
		SvcType:   settings.ServerType,
		Prefix:    settings.Prefix,
		WorkerNum: settings.WorkerNum,
		QueueCap:  1024,
	})
	if rpcEnabled {
		if err := m.bus.EnableJetStreamRPC(fnats.IJetStream(m.asm.JetStream), rpcCfg); err != nil {
			return err
		}
		slog.Info("nats mod: jetstream rpc enabled",
			"request_stream", rpcCfg.RequestStream,
			"response_stream", rpcCfg.ResponseStream,
		)
	}
	if reliableEnabled {
		redisClient, ok := app.Lookup[fredis.IRedis](r, mods.ModRedis)
		if !ok || redisClient == nil {
			return errors.New("nats reliable bus requires redis mod")
		}
		m.bus.EnableReliable(bus.NewRedisReliableStore(redisClient, reliable), reliable)
	}
	if err := bus.RegisterAdminCommands(adminReg, m.bus); err != nil {
		return err
	}

	return mods.RegisterAll(r,
		mods.Capability{Name: mods.ModNats, Value: fnats.IClient(m.asm.Client)},
		mods.Capability{Name: mods.ModNatsJetStream, Value: fnats.IJetStream(m.asm.JetStream)},
		mods.Capability{Name: mods.ModNatsRpc, Value: fnats.IRpc(m.asm.RPC)},
		mods.Capability{Name: mods.ModBus, Value: bus.IBus(m.bus)},
	)
}

func (m *NatsMod) Start() error {
	if m.bus == nil {
		return nil
	}
	if err := m.bus.Start(); err != nil {
		return err
	}
	slog.Info("nats mod: started")
	return nil
}

func (m *NatsMod) Stop() {
	if err := m.StopWithContext(fctx.BaseContext()); err != nil {
		slog.Error("nats mod: stop failed", "err", err)
	}
}

// StopWithContext 先停 Bus、再关 Assembly；排空超出预算时保留对象供再次停止，见下方注释。
// 并发调用串行执行，后到者在自己的 ctx 内等第一个做完（RR-20261006-10；旧实现读写 m.bus / m.asm
// 不加锁，并发调用有数据竞争）。
func (m *NatsMod) StopWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	if err := m.stopSerial.Lock(ctx); err != nil {
		return err
	}
	defer m.stopSerial.Unlock()
	var err error
	if m.bus != nil {
		if stopper, ok := any(m.bus).(interface{ StopWithContext(context.Context) error }); ok {
			if stopErr := stopper.StopWithContext(ctx); stopErr != nil {
				if busDrainPending(stopErr) {
					// Bus 的 handler 仍在运行、连接仍在使用；保留 bus / asm，
					// 再次停止会继续等同一次排空（RR-20261004-07）。
					return stopErr
				}
				// 终态错误（如退订失败）：Bus 已不会再有进展，照常关闭 Assembly。
				err = errors.Join(err, stopErr)
			}
		} else {
			m.bus.Stop()
		}
		m.bus = nil
	}
	if m.asm != nil {
		if closeErr := m.asm.Close(ctx); closeErr != nil {
			err = errors.Join(err, closeErr)
			if assemblyClosePending(closeErr) {
				// RPC 回调仍在运行、连接仍在使用；保留 asm，再次停止会继续
				// 等同一次回调排空。
				slog.Warn("nats mod: drain interrupted", "err", closeErr)
				return err
			}
			// 连接已被硬关闭（drain 超预算，或连接早已关闭 / 正在重连）：报告
			// 错误并释放引用。旧实现在这里也保留 asm，重试只能拿到 nats.go 的
			// ErrConnectionClosed，永远失败、引用永不置空（RR-20261004-08）。
			slog.Warn("nats mod: connection closed before drain finished", "err", closeErr)
		}
		m.mu.Lock()
		m.asm = nil
		m.mu.Unlock()
	}
	slog.Info("nats mod: stopped")
	return err
}

// busDrainPending reports whether a Bus stop error only means the caller's
// budget ran out. Bus.StopWithContext returns a ctx error exactly when its pool
// has not drained yet and keeps that drain for a later call; every other error
// is terminal.
func busDrainPending(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// assemblyClosePending reports whether Assembly.Close only ran out of budget
// waiting for RPC callbacks and still owns the connection for a later Close.
// ErrClosedUndrained may wrap a ctx error as well, but by then the connection
// is closed and nothing is left to drain.
func assemblyClosePending(err error) bool {
	return busDrainPending(err) && !errors.Is(err, natsdriver.ErrClosedUndrained)
}
