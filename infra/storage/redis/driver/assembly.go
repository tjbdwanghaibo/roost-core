package driver

import (
	"context"
	"errors"
	"fmt"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// Assembly is the complete Redis capability set a process publishes: the
// contract client. It is what the wiring RedisMod hands out; the Mod itself
// only parses configuration, registers capabilities and forwards lifecycle
// calls.
type Assembly struct {
	Client *Client
}

// Assemble builds the client. It refuses a configuration that cannot connect
// anywhere, like NewClient.
func Assemble(cfg *fredis.Config) (*Assembly, error) {
	if cfg == nil {
		return nil, fmt.Errorf("redis: configuration is required")
	}
	if cfg.Addr == "" && !cfg.IsCluster() {
		return nil, fmt.Errorf("redis: addr or cluster addrs are required")
	}
	client := NewRedisClient(cfg)
	return &Assembly{Client: client}, nil
}

// Ping verifies connectivity; the Mod calls it at Start and from its health
// check.
func (a *Assembly) Ping(ctx context.Context) error {
	if a == nil || a.Client == nil {
		return errors.New("redis: not assembled")
	}
	return a.Client.Ping(ctx)
}

// Close releases the connection pool.
func (a *Assembly) Close() error {
	if a == nil || a.Client == nil {
		return nil
	}
	return a.Client.Close()
}
