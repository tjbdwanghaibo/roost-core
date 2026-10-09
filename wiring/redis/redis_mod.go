package redis

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"log/slog"
	"sync"
	"time"

	"github.com/spf13/viper"
)

// RedisMod implements app.Mod for Redis connectivity. It parses configuration,
// asks core to assemble the client and lock factory, publishes them as
// capabilities and forwards lifecycle calls; it holds no driver handles (P3b).
type RedisMod struct {
	// mu 保护 asm：停止入口可能并发调用，健康检查也可能与停止并发（RR-20261006-10）。
	mu  sync.Mutex
	asm *redisdriver.Assembly
	cfg *fredis.Config
}

// assembly 返回当前持有的 Assembly；停止之后为 nil。
func (m *RedisMod) assembly() *redisdriver.Assembly {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.asm
}

func NewRedisMod() *RedisMod {
	return &RedisMod{}
}

func (m *RedisMod) Name() app.ModName { return mods.ModRedis }

// ClusterConfig 是 redis.cluster_addrs 的声明。Redis Mod、单实例锁的连接与各服务 Mod 的 Cluster hash tag 校验
// （mods.ValidateClusterKeyPrefix）都按它判断是不是 Cluster，所以单独成一个结构体，服务 Mod 匿名嵌入它共用这份声明。
// 逗号分隔的字符串与 YAML 列表都接受，每项去掉两端空白（RR-20261005-NC-190）。
type ClusterConfig struct {
	ClusterAddrs []string `config:"redis.cluster_addrs" help:"Redis Cluster 种子地址（逗号分隔或 YAML 列表）；写了就按 Cluster 连接，redis.addr 不再使用"`
}

// Config 是 redis.* 的声明（维护者决定 A4 ①），Redis Mod 与单实例锁的 SingletonStore 共用。
// redis.addr 缺省时 Redis Mod 兜底 localhost:6379，SingletonStore 则报错。
type Config struct {
	Addr         string `config:"redis.addr" example:"127.0.0.1:6379" help:"单机 Redis 地址"`
	Password     string `config:"redis.password" example:""`
	DB           int    `config:"redis.db" min:"0" example:"0"`
	PoolSize     int    `config:"redis.pool_size" min:"0" example:"32" help:"连接池大小，0 取驱动默认"`
	MinIdleConns int    `config:"redis.min_idle_conns" min:"0" example:"4" help:"最少空闲连接，0 取驱动默认"`
	ClusterConfig
}

// ValidateConfig：生产环境必须写 redis.addr 或 redis.cluster_addrs。两种写法都能接上 Redis；Cluster 配置优先，
// 只为过校验而写的 addr 不起作用（RR-20261006-28）。以前这条按服务类型写在 app 的生产校验里，现在跟着 Redis 配置走：
// 读 Redis 配置的进程（Redis Mod、单实例锁）都受它约束。
func (c *Config) ValidateConfig(production bool) error {
	if production && c.Addr == "" && len(c.ClusterAddrs) == 0 {
		return errors.New("config: production requires redis.addr or redis.cluster_addrs")
	}
	return nil
}

// connection 把声明读出的值转成连接配置；pool_size / min_idle_conns 为 0 时取驱动默认值。
func (c Config) connection() *fredis.Config {
	out := fredis.DefaultConfig(c.Addr)
	out.Password = c.Password
	out.DB = c.DB
	if c.PoolSize > 0 {
		out.PoolSize = c.PoolSize
	}
	if c.MinIdleConns > 0 {
		out.MinIdleConns = c.MinIdleConns
	}
	out.ClusterAddrs = c.ClusterAddrs
	return out
}

// ConfigSchema 声明 redis.*。
func (m *RedisMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(Config{}) }

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

// redisConfig 按声明读 redis.*：写错类型或超出范围的值点名报错。
func redisConfig(cfg *viper.Viper) (*fredis.Config, error) {
	var settings Config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return nil, fmt.Errorf("redis mod: %w", err)
	}
	return settings.connection(), nil
}

func (m *RedisMod) Provide(r *app.Registry) error {
	asm, err := redisdriver.Assemble(m.cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.asm = asm
	m.mu.Unlock()
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return fmt.Errorf("redis mod: capability %q not found", mods.ModHealth)
	}
	healthReg.Register("redis", health.CheckerFunc(func(ctx context.Context) health.Result {
		asm := m.assembly()
		if asm == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := asm.Ping(checkCtx); err != nil {
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
//
// 并发调用串行执行，后到者等第一个关完再返回 nil；go-redis 的 Close 不等在途命令，持锁时间很短
// （RR-20261006-10；旧实现读写 m.asm 不加锁，并发调用有数据竞争）。
func (m *RedisMod) StopWithContext(_ context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.asm == nil {
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
