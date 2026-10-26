package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// AuthorityConfig 是 redis_authority.* 的声明：权威 Redis 的连接。Saga 协调器（以及同进程的 Store Service）的权威存储在
// 这个实例上。它可以与 redis.addr 是同一个实例（开发环境方便），但生产上推荐独立部署：appendfsync always 让每次写都 fsync，
// noeviction 不适合缓存。实例必须满足部署契约（单主、appendonly yes、appendfsync always、no-appendfsync-on-rewrite no、
// maxmemory-policy noeviction），AuthorityMod.Start 的探针强制检查。没有 cluster_addrs：不支持 Cluster。
type AuthorityConfig struct {
	Addr         string `config:"redis_authority.addr" example:"127.0.0.1:6379" help:"权威 Redis 单主地址（AOF always、noeviction）；可以与 redis.addr 相同，启动探针检查部署契约"`
	Password     string `config:"redis_authority.password" secret:"optional" example:""`
	DB           int    `config:"redis_authority.db" min:"0" example:"0"`
	PoolSize     int    `config:"redis_authority.pool_size" default:"64" min:"1" example:"64" help:"连接池大小，必须不小于本进程 saga 协调器、发布器、各消费者与 Store Service 分区循环的并发之和"`
	MinIdleConns int    `config:"redis_authority.min_idle_conns" default:"8" min:"0" example:"8"`
}

// ValidateConfig：生产环境必须写 redis_authority.addr；缺省时开发环境取 localhost:6379。
func (c *AuthorityConfig) ValidateConfig(production bool) error {
	if production && c.Addr == "" {
		return errors.New("config: production requires redis_authority.addr")
	}
	return nil
}

func (c AuthorityConfig) connection() *fredis.Config {
	out := fredis.DefaultConfig(c.Addr)
	if out.Addr == "" {
		out.Addr = "localhost:6379"
	}
	out.Password = c.Password
	out.DB = c.DB
	out.PoolSize = c.PoolSize
	out.MinIdleConns = c.MinIdleConns
	return out
}

// LoadAuthorityConfig 按 AuthorityMod 的声明读 redis_authority.*（运维工具与 saga Mod 的连接池校验用，不另抄缺省）。
func LoadAuthorityConfig(cfg *viper.Viper) (AuthorityConfig, error) {
	var settings AuthorityConfig
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return AuthorityConfig{}, fmt.Errorf("redis authority: %w", err)
	}
	return settings, nil
}

// NewAuthorityClient 按 redis_authority.* 建一个权威 Redis 客户端（Mod 生命周期之外的工具用，调用方负责 Close）。
func NewAuthorityClient(settings AuthorityConfig) (*redisdriver.Client, error) {
	conn := settings.connection()
	if conn.Addr == "" {
		return nil, errors.New("redis authority: addr is required")
	}
	return redisdriver.NewRedisClient(conn), nil
}

// AuthorityMod 解析 redis_authority.*、建权威 Redis 客户端并发布为 mods.ModRedisAuthority；Start 跑部署契约探针
// （fredis.CheckAuthority），不满足就拒绝启动。它不持有业务键：键前缀与元数据守卫在使用方（saga 的 RedisStore）。
type AuthorityMod struct {
	mu       sync.Mutex
	client   *redisdriver.Client
	settings AuthorityConfig
}

// NewAuthorityMod 返回权威 Redis Mod。
func NewAuthorityMod() *AuthorityMod { return &AuthorityMod{} }

func (m *AuthorityMod) Name() app.ModName { return mods.ModRedisAuthority }

// ConfigSchema 声明 redis_authority.*。
func (m *AuthorityMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(AuthorityConfig{}) }

func (m *AuthorityMod) Init(cfg *viper.Viper) error {
	settings, err := LoadAuthorityConfig(cfg)
	if err != nil {
		return err
	}
	m.settings = settings
	return nil
}

func (m *AuthorityMod) Provide(r *app.Registry) error {
	client, err := NewAuthorityClient(m.settings)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.client = client
	m.mu.Unlock()
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return fmt.Errorf("redis authority mod: capability %q not found", mods.ModHealth)
	}
	healthReg.Register("redis_authority", health.CheckerFunc(func(ctx context.Context) health.Result {
		m.mu.Lock()
		client := m.client
		m.mu.Unlock()
		if client == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := client.Ping(checkCtx); err != nil {
			return health.Result{Status: health.StatusFail, Message: "ping failed", Err: err}
		}
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))
	return mods.RegisterAll(r, mods.Capability{Name: mods.ModRedisAuthority, Value: fredis.IRedis(client)})
}

// authorityProbeKey 是探针写的键（一个计数器）；同一实例上的多个部署共用它无害。
const authorityProbeKey = "roost:redis_authority:probe"

// Start 跑部署契约探针：单主、AOF always、noeviction、同连接 WAITAOF 确认本地落盘。不满足返回错误，进程不启动。
func (m *AuthorityMod) Start() error {
	m.mu.Lock()
	client := m.client
	m.mu.Unlock()
	if client == nil {
		return errors.New("redis authority mod: not provided")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fredis.CheckAuthority(ctx, client, authorityProbeKey, time.Second); err != nil {
		return fmt.Errorf("redis authority mod: %s: %w", m.settings.connection().Addr, err)
	}
	slog.Info("redis authority mod: connected; single primary with appendfsync always", "addr", m.settings.connection().Addr)
	return nil
}

func (m *AuthorityMod) Stop() {
	if err := m.StopWithContext(context.Background()); err != nil {
		slog.Error("redis authority mod: close failed", "err", err)
	}
}

// StopWithContext 关闭连接池（与 RedisMod 相同口径：只关一次，错误只报给第一次，之后返回 nil）。
func (m *AuthorityMod) StopWithContext(_ context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client == nil {
		return nil
	}
	client := m.client
	m.client = nil
	if err := client.Close(); err != nil {
		return fmt.Errorf("redis authority mod: close (the connection pool is closed regardless): %w", err)
	}
	return nil
}

var (
	_ app.Mod                   = (*AuthorityMod)(nil)
	_ app.ModStopperWithContext = (*AuthorityMod)(nil)
)
