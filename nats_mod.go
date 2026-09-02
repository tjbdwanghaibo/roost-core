package nats

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/cube-core/admin"
	"github.com/tjbdwanghaibo/cube-core/app"
	"github.com/tjbdwanghaibo/cube-core/bus"
	fctx "github.com/tjbdwanghaibo/cube-core/fctx"
	"github.com/tjbdwanghaibo/cube-core/health"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	fredis "github.com/tjbdwanghaibo/cube-core/redis"
	"github.com/tjbdwanghaibo/cube-kit/mods"
	"log/slog"
	"strings"

	"github.com/spf13/viper"
)

// NatsMod implements app.Mod for NATS connectivity.
// It creates IClient, IRpc, and Bus instances and registers them in the Registry.
type NatsMod struct {
	client    *natsClient
	jetStream *jetStreamClient
	rpc       *rpcClient
	bus       *bus.Bus
	codec     bus.Codec
	cfg       *fnats.Config
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
	return nil
}

func (m *NatsMod) Provide(r *app.Registry) error {
	client, err := newNatsClient(m.cfg)
	if err != nil {
		return err
	}
	m.client = client
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return errors.New("nats mod: health registry not found")
	}
	adminReg, ok := app.Lookup[*admin.Registry](r, mods.ModAdmin)
	if !ok || adminReg == nil {
		return errors.New("nats mod: admin registry not found")
	}
	healthReg.Register("nats", health.CheckerFunc(func(context.Context) health.Result {
		if m.client == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		if !m.client.Connected() {
			return health.Result{Status: health.StatusFail, Message: "not connected"}
		}
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))
	jetStream, err := newJetStreamClient(client)
	if err != nil {
		return err
	}
	m.jetStream = jetStream

	policy := fnats.DefaultRetryPolicy()
	m.rpc = newRpcClient(client, policy, 4)

	// Create bus
	sid := r.Config().GetInt32("sid")
	svcType := r.Config().GetString("server_type")
	prefix := r.Config().GetString("nats.prefix")
	if prefix == "" {
		prefix = "cube"
	}
	workerNum := r.Config().GetInt("nats.worker_num")
	if workerNum <= 0 {
		workerNum = 8
	}

	m.bus = bus.New(m.client, m.rpc, m.codec, bus.Config{
		Sid:       sid,
		SvcType:   svcType,
		Prefix:    prefix,
		WorkerNum: workerNum,
		QueueCap:  1024,
	})
	if rpcCfg, enabled := jetStreamRPCConfigFromViper(r.Config()); enabled {
		if err := m.bus.EnableJetStreamRPC(fnats.IJetStream(m.jetStream), rpcCfg); err != nil {
			return err
		}
		slog.Info("nats mod: jetstream rpc enabled",
			"request_stream", rpcCfg.RequestStream,
			"response_stream", rpcCfg.ResponseStream,
		)
	}
	if r.Config().GetBool("nats.reliable.enabled") {
		redisClient, ok := app.Lookup[fredis.IRedis](r, mods.ModRedis)
		if !ok || redisClient == nil {
			return errors.New("nats reliable bus requires redis mod")
		}
		m.bus.EnableReliable(bus.NewRedisReliableStore(redisClient, bus.ReliableConfig{
			Enabled:  true,
			Prefix:   r.Config().GetString("nats.reliable.prefix"),
			InboxTTL: r.Config().GetDuration("nats.reliable.inbox_ttl"),
			DLQTTL:   r.Config().GetDuration("nats.reliable.dlq_ttl"),
		}), bus.ReliableConfig{
			Enabled:  true,
			Prefix:   r.Config().GetString("nats.reliable.prefix"),
			InboxTTL: r.Config().GetDuration("nats.reliable.inbox_ttl"),
			DLQTTL:   r.Config().GetDuration("nats.reliable.dlq_ttl"),
		})
	}
	if err := bus.RegisterAdminCommands(adminReg, m.bus); err != nil {
		return err
	}

	return mods.RegisterAll(r,
		mods.Capability{Name: mods.ModNats, Value: fnats.IClient(m.client)},
		mods.Capability{Name: mods.ModNatsJetStream, Value: fnats.IJetStream(m.jetStream)},
		mods.Capability{Name: mods.ModNatsRpc, Value: fnats.IRpc(m.rpc)},
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
			err = errors.Join(err, stopper.StopWithContext(ctx))
		} else {
			m.bus.Stop()
		}
		m.bus = nil
	}
	if m.rpc != nil {
		m.rpc.Stop()
		m.rpc = nil
	}
	if m.client != nil {
		if drainErr := m.client.DrainWithContext(ctx); drainErr != nil {
			err = errors.Join(err, drainErr)
			slog.Warn("nats mod: drain interrupted", "err", drainErr)
			m.client.Close()
		}
		m.client = nil
	}
	m.jetStream = nil
	slog.Info("nats mod: stopped")
	return err
}

func jetStreamRPCConfigFromViper(cfg *viper.Viper) (bus.JetStreamRPCConfig, bool) {
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
		AckWait:        cfg.GetDuration("nats.rpc.ack_wait"),
		MaxDeliver:     cfg.GetInt("nats.rpc.max_deliver"),
		RequestTTL:     cfg.GetDuration("nats.rpc.request_ttl"),
		CallTimeout:    cfg.GetDuration("nats.rpc.call_timeout"),
		StreamMaxAge:   cfg.GetDuration("nats.rpc.stream_max_age"),
		Duplicates:     cfg.GetDuration("nats.rpc.duplicates"),
		Replicas:       cfg.GetInt("nats.rpc.replicas"),
		MaxBytes:       cfg.GetInt64("nats.rpc.max_bytes"),
		SetupTimeout:   cfg.GetDuration("nats.rpc.setup_timeout"),
	}, true
}
