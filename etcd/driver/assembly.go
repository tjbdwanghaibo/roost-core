package driver

import (
	"context"
	"errors"
	"fmt"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

// Assembly is the complete etcd capability set a process publishes: the
// contract client, service discovery and the election factory, all on one
// clientv3 connection. The kit EtcdMod only parses configuration, registers
// the three capabilities and forwards lifecycle calls (P3b); it never touches
// the raw clientv3 handle.
type Assembly struct {
	Client    *Client
	Discovery *Discovery
	Election  *ElectionFactory

	endpoints []string
}

// Assemble connects once and builds discovery and elections on that
// connection. clientv3 does not dial eagerly, so a bad endpoint surfaces at
// Ping / Start, not here.
func Assemble(cfg *fetcd.Config) (*Assembly, error) {
	if cfg == nil || len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("etcd: configuration with at least one endpoint is required")
	}
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	discovery := NewDiscovery(client.cli, cfg.ServicePrefix, cfg.LeaseTTL)
	discovery.SetRetryIntervals(cfg.RegisterRetryMinInterval, cfg.RegisterRetryMaxInterval)
	return &Assembly{
		Client:    client,
		Discovery: discovery,
		Election:  NewElectionFactory(client.cli),
		endpoints: append([]string(nil), cfg.Endpoints...),
	}, nil
}

// Ping asks the first configured endpoint for its status — the probe the Mod's
// health check and Start have always used.
func (a *Assembly) Ping(ctx context.Context) error {
	if a == nil || a.Client == nil || a.Client.cli == nil {
		return errors.New("etcd: not assembled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := a.Client.cli.Status(ctx, a.endpoints[0])
	return err
}

// Start probes the cluster and, when info names a service type, registers
// this process for discovery. A nil info or an empty service type means "no
// registration", which is how tools and tests use the Mod.
func (a *Assembly) Start(ctx context.Context, info *fetcd.ServiceInfo) error {
	if err := a.Ping(ctx); err != nil {
		return err
	}
	if info == nil || info.ServiceType == "" {
		return nil
	}
	return a.Discovery.Register(ctx, info)
}

// Close deregisters (revoking the lease and its keys) and closes the
// connection. Both errors are reported.
func (a *Assembly) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	if a.Discovery != nil {
		err = errors.Join(err, a.Discovery.Deregister(ctx))
	}
	if a.Client != nil {
		err = errors.Join(err, a.Client.Close())
	}
	return err
}
