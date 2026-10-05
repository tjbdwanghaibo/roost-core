package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/admin"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/bus"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"

	"github.com/spf13/viper"
)

// NatsMod implements app.Mod for NATS connectivity. Core assembles the
// connection, JetStream and RPC client; the Mod parses configuration, builds
// the bus from registry configuration on top of them, publishes the
// capabilities and forwards lifecycle calls (P3b).
type NatsMod struct {
	asm   *natsdriver.Assembly
	bus   *bus.Bus
	codec bus.Codec
	cfg   *fnats.Config
	extra natsdriver.ClientOptions
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

func (m *NatsMod) Init(cfg *viper.Viper) error {
	url := cfg.GetString("nats.url")
	if url == "" {
		url = "nats://localhost:4222"
	}
	m.cfg = fnats.DefaultConfig(url)
	// nats.ignore_discovered_servers: stay on the configured URLs instead of
	// following cluster gossip — for proxies, NAT, and fault injection.
	ignoreDiscovered, err := app.ConfigBool(cfg, "nats.ignore_discovered_servers")
	if err != nil {
		return fmt.Errorf("nats mod: %w", err)
	}
	m.extra = natsdriver.ClientOptions{IgnoreDiscoveredServers: ignoreDiscovered}
	return nil
}

func (m *NatsMod) Provide(r *app.Registry) error {
	asm, err := natsdriver.Assemble(m.cfg, m.extra)
	if err != nil {
		return err
	}
	m.asm = asm
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return errors.New("nats mod: health registry not found")
	}
	adminReg, ok := app.Lookup[*admin.Registry](r, mods.ModAdmin)
	if !ok || adminReg == nil {
		return errors.New("nats mod: admin registry not found")
	}
	healthReg.Register("nats", health.CheckerFunc(func(context.Context) health.Result {
		if m.asm == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		if !m.asm.Connected() {
			return health.Result{Status: health.StatusFail, Message: "not connected"}
		}
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))
	// Create bus。类型化的键先全部严格读完（维护者决定 A4），写错类型时在建 bus 之前报错。
	read := app.NewConfigReader(r.Config())
	sid := r.Config().GetInt32("sid")
	svcType := r.Config().GetString("server_type")
	prefix := r.Config().GetString("nats.prefix")
	if prefix == "" {
		prefix = "roost"
	}
	workerNum := read.Int("nats.worker_num")
	if workerNum <= 0 {
		workerNum = 8
	}
	rpcCfg, rpcEnabled := jetStreamRPCConfigFromViper(r.Config(), read)
	reliable := bus.ReliableConfig{
		Enabled:  true,
		Prefix:   r.Config().GetString("nats.reliable.prefix"),
		InboxTTL: read.Duration("nats.reliable.inbox_ttl"),
		DLQTTL:   read.Duration("nats.reliable.dlq_ttl"),
	}
	reliableEnabled := read.Bool("nats.reliable.enabled")
	if err := read.Err(); err != nil {
		return fmt.Errorf("nats mod: %w", err)
	}

	m.bus = bus.New(m.asm.Client, m.asm.RPC, m.codec, bus.Config{
		Sid:       sid,
		SvcType:   svcType,
		Prefix:    prefix,
		WorkerNum: workerNum,
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

func (m *NatsMod) StopWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
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
		m.asm = nil
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

// jetStreamRPCConfigFromViper 读取 JetStream RPC 配置；类型化的键经 read 严格读取，错误由调用方从 read.Err 取。
func jetStreamRPCConfigFromViper(cfg *viper.Viper, read *app.ConfigReader) (bus.JetStreamRPCConfig, bool) {
	if cfg == nil {
		return bus.JetStreamRPCConfig{}, false
	}
	transport := strings.ToLower(strings.TrimSpace(cfg.GetString("nats.rpc.transport")))
	enabled := transport == "jetstream" || transport == "js"
	if !enabled {
		return bus.JetStreamRPCConfig{}, false
	}
	return bus.JetStreamRPCConfig{
		RequestStream:  cfg.GetString("nats.rpc.request_stream"),
		ResponseStream: cfg.GetString("nats.rpc.response_stream"),
		AckWait:        read.Duration("nats.rpc.ack_wait"),
		MaxDeliver:     read.Int("nats.rpc.max_deliver"),
		RequestTTL:     read.Duration("nats.rpc.request_ttl"),
		CallTimeout:    read.Duration("nats.rpc.call_timeout"),
		StreamMaxAge:   read.Duration("nats.rpc.stream_max_age"),
		Duplicates:     read.Duration("nats.rpc.duplicates"),
		Replicas:       read.Int("nats.rpc.replicas"),
		MaxBytes:       read.Int64("nats.rpc.max_bytes"),
		SetupTimeout:   read.Duration("nats.rpc.setup_timeout"),
	}, true
}
