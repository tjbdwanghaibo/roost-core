package global

import (
	"fmt"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Mod wires the routing service into an app and registers it as a
// capability.
//
// It used to wire two services and publish two capabilities: routing/leases
// and cross-server activity coordination lived in one package. They shared no
// state — routing answers "where does this game belong",
// activity coordination answers "has every game reached the phase yet" — and
// app.Service is one per process, so they were always two deployments. They
// are now two packages; the activity Mod is activity.NewMod.
//
// The Mod needs no policy collaborator: every decision here is a
// compare-and-set against state, not a judgement about a caller.
type Mod struct {
	metrics servicemetrics.Reporter

	prefix string

	service *Service
}

// NewMod returns a global Mod.
func NewMod(reporter servicemetrics.Reporter) *Mod {
	return &Mod{metrics: reporter}
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// config 是 global.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	Global struct {
		KeyPrefix string `config:"key_prefix" required:"true" example:"roost:{project}:global" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
	} `config:"global"`
}

// ConfigSchema 声明 global.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	global:
//	  key_prefix: roost:global   # required, no default
//
// lease_ttl is no longer read: the lease API it configured was removed, and
// viper ignores the key if an older configuration still sets it.
//
// The activity settings that used to live under this key — reservation_ttl,
// grace_window, dispatch_attempts, dispatch_backoff — moved with the service,
// to activity.Mod's own section.
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("global mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Global
	if err := mods.CheckKeyPrefix("global", c.KeyPrefix); err != nil {
		return err
	}
	m.prefix = c.KeyPrefix
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	stores, err := NewRedisStores(client, m.prefix)
	if err != nil {
		return err
	}
	service, err := New(Config{Routes: stores.Routes, Metrics: m.metrics})
	if err != nil {
		return err
	}
	m.service = service
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface consumers look up, and the owner-only name the
	// Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(service)...)
}

// Start implements app.Mod. Nothing is started here: the routing state has no
// background work (see Server.run).
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
