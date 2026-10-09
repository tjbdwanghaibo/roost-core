package session

import domain "github.com/tjbdwanghaibo/roost-core/service/session"

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// Mod wires a session Service into an app and registers it as a capability.
//
// The Releaser is a required constructor argument with no default, and that is
// the load-bearing decision in this file. A run allocates external resources —
// a scene, a replica, a reserved shard — and only the caller knows how to hand
// them back. A default that did nothing would compile, start, pass a smoke
// test, and leak every resource every run ever took: exactly the failure this
// package was extracted to prevent, reintroduced by a convenience.
type Mod struct {
	release domain.Releaser
	metrics servicemetrics.Reporter

	prefix     string
	ttl        time.Duration
	requestTTL time.Duration
	service    *domain.Service
	owners     domain.OwnerSource
}

// NewMod returns a session Mod. release is required.
func NewMod(release domain.Releaser, reporter servicemetrics.Reporter, options ...ModOption) *Mod {
	m := &Mod{release: release, metrics: reporter}
	for _, option := range options {
		option(m)
	}
	return m
}

// Name implements app.Mod.
// Name implements app.Mod. It returns the generated CapabilityName, so the
// Mod's name and the capability it publishes are one fact rather than two.
func (m *Mod) Name() app.ModName { return domain.CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// config 是 session.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	kitredis.ClusterConfig
	Session struct {
		KeyPrefix  string        `config:"key_prefix" required:"true" example:"roost:{project}:session" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
		RunTTL     time.Duration `config:"run_ttl" default:"30m" min:"1s" example:"30m"`
		RequestTTL time.Duration `config:"request_ttl" required:"true" min:"1ns" example:"1h" help:"Required, with no default: past the ttl a retried Enter is indistinguishable from a new one,\nand the caller gets a second run."`
	} `config:"session"`
}

// ConfigSchema 声明 session.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	session:
//	  key_prefix: roost:session   # required, no default
//	  run_ttl: 30m                # optional, defaults to DefaultTTL
//	  request_ttl: 1h             # required; see below
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("session mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Session
	if err := mods.CheckKeyPrefix("session", c.KeyPrefix); err != nil {
		return err
	}
	if m.release == nil {
		return fmt.Errorf("session mod: a releaser is required; a run that allocates external " +
			"resources and cannot release them is the leak this package prevents, and a default " +
			"that did nothing would make leaking the out-of-the-box behaviour")
	}
	m.prefix, m.ttl, m.requestTTL = c.KeyPrefix, c.RunTTL, c.RequestTTL
	if err := mods.ValidateClusterKeyPrefix(settings.ClusterAddrs, "session", c.KeyPrefix); err != nil {
		return err
	}
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	stores, err := domain.NewRedisStores(client, domain.RedisConfig{
		Prefix: m.prefix, RequestTTL: m.requestTTL,
	})
	if err != nil {
		return fmt.Errorf("session mod: %w", err)
	}
	service, err := domain.New(domain.Config{
		Runs: stores.Runs, Claims: stores.Claims, Requests: stores.Requests,
		Release: m.release, TTL: m.ttl, Metrics: m.metrics,
		Owners: m.owners,
		// run 的开始、截止与结束时间发给客户端，是玩法计时：业务时钟（D-L3）。
		Now: app.BusinessClock(r).Now,
	})
	if err != nil {
		return fmt.Errorf("session mod: %w", err)
	}
	m.service = service
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface consumers look up, and the owner-only name the
	// Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(service)...)
}

// Start implements app.Mod.
//
// Nothing is started. Expired runs are resolved by Sweep, which the caller
// drives — "how often does this deployment sweep" is a deployment decision,
// and a goroutine a Mod starts silently is one nobody can see failing.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
