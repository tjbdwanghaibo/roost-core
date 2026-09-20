package redis

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/health"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-kit/mods"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// RedisMod implements app.Mod for Redis connectivity. It parses configuration,
// asks core to assemble the client and lock factory, publishes them as
// capabilities and forwards lifecycle calls; it holds no driver handles (P3b).
type RedisMod struct {
	asm *redisdriver.Assembly
	cfg *fredis.Config
}

func NewRedisMod() *RedisMod {
	return &RedisMod{}
}

func (m *RedisMod) Name() app.ModName { return mods.ModRedis }

func (m *RedisMod) Init(cfg *viper.Viper) error {
	addr := cfg.GetString("redis.addr")
	if addr == "" {
		addr = "localhost:6379"
	}
	m.cfg = fredis.DefaultConfig(addr)
	m.cfg.Password = cfg.GetString("redis.password")
	m.cfg.DB = cfg.GetInt("redis.db")

	if poolSize := cfg.GetInt("redis.pool_size"); poolSize > 0 {
		m.cfg.PoolSize = poolSize
	}
	if minIdle := cfg.GetInt("redis.min_idle_conns"); minIdle > 0 {
		m.cfg.MinIdleConns = minIdle
	}

	// Cluster mode
	clusterAddrs := cfg.GetString("redis.cluster_addrs")
	if clusterAddrs != "" {
		m.cfg.ClusterAddrs = strings.Split(clusterAddrs, ",")
	}

	return nil
}

func (m *RedisMod) Provide(r *app.Registry) error {
	asm, err := redisdriver.Assemble(m.cfg)
	if err != nil {
		return err
	}
	m.asm = asm
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return fmt.Errorf("redis mod: capability %q not found", mods.ModHealth)
	}
	healthReg.Register("redis", health.CheckerFunc(func(ctx context.Context) health.Result {
		if m.asm == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := m.asm.Ping(checkCtx); err != nil {
			return health.Result{Status: health.StatusFail, Message: "ping failed", Err: err}
		}
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))

	return mods.RegisterAll(r,
		mods.Capability{Name: mods.ModRedis, Value: fredis.IRedis(m.asm.Client)},
		mods.Capability{Name: mods.ModRedisLock, Value: fredis.IDistLockFactory(m.asm.Locks)},
	)
}

func (m *RedisMod) Start() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := m.asm.Ping(ctx); err != nil {
		return err
	}
	slog.Info("redis mod: connected", "addr", m.cfg.Addr, "cluster", m.cfg.IsCluster())
	return nil
}

func (m *RedisMod) Stop() {
	if err := m.StopWithContext(context.Background()); err != nil {
		slog.Error("redis mod: close failed", "err", err)
	}
}

func (m *RedisMod) StopWithContext(_ context.Context) error {
	if m == nil || m.asm == nil {
		return nil
	}
	err := m.asm.Close()
	if err == nil {
		slog.Info("redis mod: closed")
		m.asm = nil
	}
	return err
}

var _ app.ModStopperWithContext = (*RedisMod)(nil)
