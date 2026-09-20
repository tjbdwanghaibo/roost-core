package etcd

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
	etcddriver "github.com/tjbdwanghaibo/roost-core/etcd/driver"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-kit/mods"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// EtcdMod implements app.Mod for etcd connectivity. It parses configuration,
// asks core to assemble client / discovery / elections on one connection,
// publishes them as capabilities and forwards lifecycle calls; it holds no
// raw clientv3 handle (P3b).
type EtcdMod struct {
	asm *etcddriver.Assembly
	cfg *fetcd.Config

	// service info for auto-registration
	serviceInfo *fetcd.ServiceInfo
}

func NewEtcdMod() *EtcdMod {
	return &EtcdMod{}
}

func (m *EtcdMod) Name() app.ModName { return mods.ModEtcd }

func (m *EtcdMod) Init(cfg *viper.Viper) error {
	endpoints := cfg.GetString("etcd.endpoints")
	if endpoints == "" {
		endpoints = "localhost:2379"
	}
	eps := strings.Split(endpoints, ",")

	m.cfg = fetcd.DefaultConfig(eps)
	m.cfg.Username = cfg.GetString("etcd.username")
	m.cfg.Password = cfg.GetString("etcd.password")

	if prefix := cfg.GetString("etcd.service_prefix"); prefix != "" {
		m.cfg.ServicePrefix = prefix
	}
	if ttl := cfg.GetInt64("etcd.lease_ttl"); ttl > 0 {
		m.cfg.LeaseTTL = ttl
	}
	if retryMin := cfg.GetDuration("etcd.register_retry_min_interval"); retryMin > 0 {
		m.cfg.RegisterRetryMinInterval = retryMin
	}
	if retryMax := cfg.GetDuration("etcd.register_retry_max_interval"); retryMax > 0 {
		m.cfg.RegisterRetryMaxInterval = retryMax
	}

	// Build service info for auto-registration
	sid := cfg.GetInt32("sid")
	svcType := cfg.GetString("server_type")
	addr := cfg.GetString("etcd.advertise_addr")
	if addr == "" {
		switch svcType {
		case "gate":
			addr = cfg.GetString("gate.backend_advertise_addr")
			if addr == "" {
				addr = cfg.GetString("gate.backend_addr")
			}
		case "game":
			addr = cfg.GetString("game.advertise_addr")
		}
	}

	m.serviceInfo = &fetcd.ServiceInfo{
		ServiceType: svcType,
		Sid:         sid,
		Addr:        addr,
		Metadata:    serviceMetadata(cfg, svcType, addr),
	}

	return nil
}

func serviceMetadata(cfg *viper.Viper, svcType string, addr string) map[string]string {
	metadata := map[string]string{}
	if addr != "" {
		metadata["addr"] = addr
	}
	switch svcType {
	case "gate":
		if v := cfg.GetString("gate.mode"); v != "" {
			metadata["mode"] = v
		}
		if v := cfg.GetString("gate.ws_addr"); v != "" {
			metadata["ws_addr"] = v
		}
		if v := cfg.GetString("gate.ws_path"); v != "" {
			metadata["ws_path"] = v
		}
		if v := cfg.GetString("gate.tcp_addr"); v != "" {
			metadata["tcp_addr"] = v
		}
		if v := cfg.GetString("gate.backend_addr"); v != "" && cfg.GetString("gate.mode") != "standalone" {
			metadata["backend_addr"] = v
		}
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
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
		mods.Capability{Name: mods.ModEtcdElection, Value: fetcd.IElectionFactory(m.asm.Election)},
	)
}

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
