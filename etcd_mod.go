package etcd

import (
	"context"
	"github.com/tjbdwanghaibo/cube-core/app"
	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
	"github.com/tjbdwanghaibo/cube-core/health"
	"github.com/tjbdwanghaibo/cube-kit/mods"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// EtcdMod implements app.Mod for etcd connectivity.
// Provides IEtcd, IDiscovery, and IElectionFactory via Registry.
type EtcdMod struct {
	client    *etcdClient
	discovery *discovery
	election  *electionFactory
	cfg       *fetcd.Config

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
	client, err := newEtcdClient(m.cfg)
	if err != nil {
		return err
	}
	m.client = client
	m.discovery = newDiscovery(client.cli, m.cfg.ServicePrefix, m.cfg.LeaseTTL)
	m.discovery.retryMinInterval = m.cfg.RegisterRetryMinInterval
	m.discovery.retryMaxInterval = m.cfg.RegisterRetryMaxInterval
	m.election = newElectionFactory(client.cli)
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return errors.New("etcd mod: health registry not found")
	}
	healthReg.Register("etcd", health.CheckerFunc(func(ctx context.Context) health.Result {
		if m.client == nil || m.client.cli == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := m.client.cli.Status(checkCtx, m.cfg.Endpoints[0])
		if err != nil {
			return health.Result{Status: health.StatusFail, Message: "status failed", Err: err}
		}
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))

	return errors.Join(
		r.Register(mods.ModEtcd, fetcd.IEtcd(m.client)),
		r.Register(mods.ModEtcdDiscov, fetcd.IDiscovery(m.discovery)),
		r.Register(mods.ModEtcdElection, fetcd.IElectionFactory(m.election)),
	)
}

func (m *EtcdMod) Start() error {
	// Ping etcd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := m.client.cli.Status(ctx, m.cfg.Endpoints[0])
	if err != nil {
		return err
	}

	// Auto-register service
	if m.serviceInfo.ServiceType != "" {
		if err := m.discovery.Register(context.Background(), m.serviceInfo); err != nil {
			return err
		}
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
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	if m.discovery != nil {
		err = errors.Join(err, m.discovery.Deregister(ctx))
	}
	if m.client != nil {
		m.client.Close()
	}
	slog.Info("etcd mod: stopped")
	return err
}
