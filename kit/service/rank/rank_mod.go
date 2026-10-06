package rank

import (
	"fmt"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	kitredis "github.com/tjbdwanghaibo/roost-core/kit/redis"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Mod wires a rank Store into an app and registers it as a capability.
//
// It owns INFRASTRUCTURE wiring only: the Redis client comes from the
// registry, the key prefix from configuration. Everything a caller might want
// to decide — which boards exist, who may submit to them — stays with the
// caller, because a Mod that decided those would be a library making a
// deployment's decisions.
//
// Rank needs no policy collaborator, which is why it is the smallest Mod
// here. See account's or platform's for the shape when a service requires
// something configuration cannot supply.
type Mod struct {
	metrics servicemetrics.Reporter

	prefix string
	store  *RedisStore
}

// NewMod returns a rank Mod.
//
// reporter may be nil, which means no reporting; see servicemetrics. It is a
// constructor argument rather than a config key because a Reporter is code,
// not a string.
func NewMod(reporter servicemetrics.Reporter) *Mod {
	return &Mod{metrics: reporter}
}

// Name implements app.Mod.
// Name implements app.Mod. It returns the generated CapabilityName, so the
// Mod's name and the capability it publishes are one fact rather than two
// literals — the app orders Mods by name and the registry keys on the
// capability, and a divergence would have them disagree about which Mod owns
// what.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider: the Redis capability has to
// exist before Provide runs.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// config 是 rank.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	kitredis.ClusterConfig
	Rank struct {
		KeyPrefix string `config:"key_prefix" required:"true" example:"roost:{project}:rank" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
	} `config:"rank"`
}

// ConfigSchema 声明 rank.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	rank:
//	  key_prefix: roost:rank   # required, no default
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("rank mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Rank
	if err := mods.CheckKeyPrefix("rank", c.KeyPrefix); err != nil {
		return err
	}
	if err := mods.ValidateClusterKeyPrefix(settings.ClusterAddrs, "rank", c.KeyPrefix); err != nil {
		return err
	}
	m.prefix = c.KeyPrefix
	return nil
}

// Provide builds the store and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	// 同分按“谁先达到”排序是排行规则，读业务时钟（D-L3）。
	store, err := NewRedisStore(client, RedisConfig{Prefix: m.prefix, Metrics: m.metrics, Now: app.BusinessClock(r).Now})
	if err != nil {
		return fmt.Errorf("rank mod: %w", err)
	}
	m.store = store
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface every consumer looks up, and the owner-only name
	// the Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(store)...)
}

// Start implements app.Mod. There is nothing to start: the store holds no
// goroutine and no connection of its own.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop, for the same reason.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
