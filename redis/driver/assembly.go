package driver

import (
	"context"
	"errors"
	"fmt"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// Assembly is the complete Redis capability set a process publishes: the
// contract client and the distributed-lock factory that shares its connection
// pool. It is what the kit RedisMod hands out; the Mod itself only parses
// configuration, registers capabilities and forwards lifecycle calls (P3b).
type Assembly struct {
	Client *Client
	Locks  *DistLockFactory
}

// Assemble builds the client and the lock factory on one connection pool.
// It refuses a configuration that cannot connect anywhere, like NewClient.
func Assemble(cfg *fredis.Config) (*Assembly, error) {
	if cfg == nil {
		return nil, fmt.Errorf("redis: configuration is required")
	}
	if cfg.Addr == "" && !cfg.IsCluster() {
		return nil, fmt.Errorf("redis: addr or cluster addrs are required")
	}
	client := NewRedisClient(cfg)
	return &Assembly{Client: client, Locks: NewDistLockFactory(client.rdb)}, nil
}

// Ping verifies connectivity; the Mod calls it at Start and from its health
// check.
func (a *Assembly) Ping(ctx context.Context) error {
	if a == nil || a.Client == nil {
		return errors.New("redis: not assembled")
	}
	return a.Client.Ping(ctx)
}

// Close releases the shared connection pool. Locks handed out by the factory
// stop working; callers must have released them.
func (a *Assembly) Close() error {
	if a == nil || a.Client == nil {
		return nil
	}
	return a.Client.Close()
}
