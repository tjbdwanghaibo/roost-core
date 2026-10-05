package redis

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
	"log/slog"
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
	conn, err := redisConfig(cfg)
	if err != nil {
		return err
	}
	m.cfg = conn
	if m.cfg.Addr == "" {
		m.cfg.Addr = "localhost:6379"
	}
	return nil
}

// redisConfig 把 redis.* 解析成连接配置，RedisMod 与单实例锁的 SingletonStore 共用。
// redis.addr 缺省时 Addr 留空：RedisMod 自己兜底 localhost:6379，SingletonStore 则报错。
//
// 三个整数键严格读取（维护者决定 A4 的留项）：8k、1.5、10s 这样的值点名报错，而不是被 viper 的宽松
// getter 读成 0（db 0、连接池取默认）。未设置或 ≤ 0 的 pool_size / min_idle_conns 仍取驱动默认值。
func redisConfig(cfg *viper.Viper) (*fredis.Config, error) {
	read := app.NewConfigReader(cfg)
	out := fredis.DefaultConfig(cfg.GetString("redis.addr"))
	out.Password = cfg.GetString("redis.password")
	out.DB = read.Int("redis.db")
	if poolSize := read.Int("redis.pool_size"); poolSize > 0 {
		out.PoolSize = poolSize
	}
	if minIdle := read.Int("redis.min_idle_conns"); minIdle > 0 {
		out.MinIdleConns = minIdle
	}
	if err := read.Err(); err != nil {
		return nil, fmt.Errorf("redis mod: %w", err)
	}
	// Cluster mode：逗号分隔或 YAML 列表（mods.RedisClusterAddrs，RR-20261005-NC-190）。
	out.ClusterAddrs = mods.RedisClusterAddrs(cfg)
	return out, nil
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

// StopWithContext 关闭连接池。go-redis 的 Close 不等在途命令（它们随之失败），先把连接池标记为关闭、
// 清空连接，再返回逐个关连接时遇到的第一个错误：返回错误时资源同样已经释放，再调 Close 只会得到
// “client is closed”。所以第一次 Close 之后不论结果都交出 asm，错误只报告这一次，之后的 Stop 返回 nil
// （停机契约“再调用返回 nil”，RR-20261005-NC-233；旧实现只在成功时置空，出错后的每次重试都失败）。
func (m *RedisMod) StopWithContext(_ context.Context) error {
	if m == nil || m.asm == nil {
		return nil
	}
	asm := m.asm
	m.asm = nil
	if err := asm.Close(); err != nil {
		return fmt.Errorf("redis mod: close (the connection pool is closed regardless): %w", err)
	}
	slog.Info("redis mod: closed")
	return nil
}

var _ app.ModStopperWithContext = (*RedisMod)(nil)
