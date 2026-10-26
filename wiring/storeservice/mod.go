// Package storeservice 是 Store Service 的 Mod：把权威 Redis 上的 saga 日志投影进 Mongo 冷副本，并驱逐已投影、过了保留期
// 的终态记录。可以与 saga 协调器同进程注册，也可以单独一个进程；多个实例靠分区租约分摊，正确性不依赖租约排他
// （framework/storeservice）。
package storeservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	corestore "github.com/tjbdwanghaibo/roost-core/framework/storeservice"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
)

// Config 是 store_service.* 的声明。saga 的键前缀、分区与冷副本位置读 saga.*（kitsaga.LoadLocations，与协调器同一份声明）。
type Config struct {
	app.ServiceIdentity
	StoreService struct {
		Owner         string        `config:"owner" help:"分区租约的持有者名，不写取 store-<sid>-<随机>"`
		BatchSize     int           `config:"batch_size" default:"256" min:"1" max:"10000" example:"256" help:"每批最多投影的日志条目数"`
		LeaseDuration time.Duration `config:"lease_duration" default:"10s" min:"1s" example:"10s" help:"分区租约时长，每三分之一续约一次"`
		Block         time.Duration `config:"block" default:"100ms" min:"1ms" example:"100ms" help:"没有新条目时阻塞读的等待上限"`
		MaxPartitions int           `config:"max_partitions" default:"0" min:"0" example:"0" help:"本进程最多同时处理的分区数，0 = 不限"`
	} `config:"store_service"`
}

// Mod 装配 Store Service：saga 投影 + 投影运行器。
type Mod struct {
	settings Config
	saga     kitsaga.Locations

	mu     sync.Mutex
	runner *corestore.Runner
}

// NewMod 返回 Store Service Mod。
func NewMod() *Mod { return &Mod{} }

func (m *Mod) Name() app.ModName { return mods.ModStoreService }

// DependsOn：Mongo（冷副本）与权威 Redis。
func (m *Mod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModMongo, mods.ModRedisAuthority}
}

// ConfigSchema 声明 store_service.*。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(Config{}) }

func (m *Mod) Init(cfg *viper.Viper) error {
	if err := app.LoadConfig(cfg, &m.settings); err != nil {
		return fmt.Errorf("store service: %w", err)
	}
	locations, err := kitsaga.LoadLocations(cfg)
	if err != nil {
		return fmt.Errorf("store service: %w", err)
	}
	m.saga = locations
	if strings.TrimSpace(m.settings.StoreService.Owner) == "" {
		m.settings.StoreService.Owner = fmt.Sprintf("store-%d-%s", m.settings.Sid, coresaga.NewID())
	}
	return nil
}

// CheckConfigAcrossMods：同一进程注册了权威 Redis Mod 时，redis_authority.pool_size 至少要容下本 Mod 的分区循环；saga Mod
// 也在同一进程时再加上它的并发（kitsaga.Concurrency）。不够时循环与消费者排队等连接。
func (m *Mod) CheckConfigAcrossMods(cfg *viper.Viper, registered func(app.ModName) bool) error {
	if !registered(mods.ModRedisAuthority) {
		return nil
	}
	var settings Config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return err
	}
	locations, err := kitsaga.LoadLocations(cfg)
	if err != nil {
		return err
	}
	authority, err := kitredis.LoadAuthorityConfig(cfg)
	if err != nil {
		return err
	}
	need := Concurrency(settings.StoreService.MaxPartitions, locations.Redis.Partitions)
	what := "the store service's partition loops"
	if registered(mods.ModSaga) {
		saga, err := kitsaga.Concurrency(cfg)
		if err != nil {
			return err
		}
		need += saga
		what += " + the saga concurrency (coordinator, publisher and consumer workers)"
	}
	if authority.PoolSize < need {
		return fmt.Errorf("redis_authority.pool_size (%d) is smaller than the concurrency on it (%d = %s); raise the pool, lower the workers or set store_service.max_partitions",
			authority.PoolSize, need, what)
	}
	return nil
}

// Concurrency 是 Store Service 在权威 Redis 连接池上的并发：每个持有的分区一个循环。
func Concurrency(maxPartitions, partitions int) int {
	if partitions <= 0 {
		partitions = coresaga.DefaultRedisStoreOptions().Partitions
	}
	if maxPartitions > 0 && maxPartitions < partitions {
		return maxPartitions
	}
	return partitions
}

func (m *Mod) Provide(registry *app.Registry) error {
	mongoClient, ok := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
	if !ok || mongoClient == nil {
		return fmt.Errorf("store service mod: capability %q not found", mods.ModMongo)
	}
	authority, ok := app.Lookup[fredis.IRedis](registry, mods.ModRedisAuthority)
	if !ok || authority == nil {
		return fmt.Errorf("store service mod: capability %q not found; add wiring/redis.NewAuthorityMod()", mods.ModRedisAuthority)
	}
	copy, err := coresaga.NewMongoStore(mongoClient, m.saga.Store)
	if err != nil {
		return err
	}
	projection, err := coresaga.NewRedisProjection(authority, copy, m.saga.Redis)
	if err != nil {
		return err
	}
	s := m.settings.StoreService
	runner, err := corestore.NewRunner(authority, corestore.Options{Owner: s.Owner, BatchSize: s.BatchSize, LeaseDuration: s.LeaseDuration, Block: s.Block, MaxPartitions: s.MaxPartitions}, projection)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.runner = runner
	m.mu.Unlock()
	healthRegistry, ok := app.Lookup[*health.Registry](registry, mods.ModHealth)
	if !ok || healthRegistry == nil {
		return fmt.Errorf("store service mod: capability %q not found", mods.ModHealth)
	}
	healthRegistry.Register("store_service", health.CheckerFunc(m.checkHealth))
	return nil
}

// Start 装载脚本并起分区循环。冷副本的索引由 saga Mod（协调器）建；单独的 Store Service 进程也建一次（幂等）。
func (m *Mod) Start() error {
	m.mu.Lock()
	runner := m.runner
	m.mu.Unlock()
	if runner == nil {
		return errors.New("store service mod: not provided")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runner.Start(ctx)
}

func (m *Mod) Stop() { _ = m.StopWithContext(context.Background()) }

// StopWithContext 停分区循环并在 ctx 内等它们退出（超时返回 ctx 错误，可用新 ctx 重试）。
func (m *Mod) StopWithContext(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	runner := m.runner
	m.mu.Unlock()
	if runner == nil {
		return nil
	}
	return runner.Stop(ctx)
}

// Runner 返回投影运行器（测试与运维观察用）。
func (m *Mod) Runner() *corestore.Runner {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runner
}

func (m *Mod) checkHealth(context.Context) health.Result {
	runner := m.Runner()
	if runner == nil {
		return health.Result{Status: health.StatusFail, Message: "not initialized"}
	}
	if paused := runner.Paused(); len(paused) > 0 {
		return health.Result{Status: health.StatusFail, Message: fmt.Sprintf("paused partitions %v: a projection hit a permanent error; repair it and delete the pause key", paused)}
	}
	var parts []string
	for name, backlog := range runner.Backlog() {
		parts = append(parts, fmt.Sprintf("%s backlog=%.0f lag=%.0fs", name, backlog[0], backlog[1]))
	}
	return health.Result{Status: health.StatusOK, Message: "running " + strings.Join(parts, " ")}
}

var (
	_ app.Mod                   = (*Mod)(nil)
	_ app.ModStopperWithContext = (*Mod)(nil)
)
