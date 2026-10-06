package directory

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Mod wires a Directory into an app and registers it as a capability.
//
// The normalizer is a constructor argument, not a config key. It decides which
// raw keys are the SAME key — "Alice", " alice " and "ALICE" — and that is a
// product decision about a namespace, not a deployment setting. Selecting it
// by string would also mean an unrecognized string had to do something, and
// every available answer there is wrong: fall back to identity and uniqueness
// quietly weakens; fall back to lower-case and a case-sensitive namespace
// quietly merges.
type Mod struct {
	normalize Normalizer
	metrics   servicemetrics.Reporter

	prefix     string
	defaultTTL time.Duration
	directory  Directory
}

// NewMod returns a directory Mod. normalize is required; see NormalizeLower
// for the common answer.
func NewMod(normalize Normalizer, reporter servicemetrics.Reporter) *Mod {
	return &Mod{normalize: normalize, metrics: reporter}
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return mods.ModDirectory }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// config 是 directory.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	Directory struct {
		KeyPrefix      string        `config:"key_prefix" required:"true" example:"roost:{project}:directory" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
		ReservationTTL time.Duration `config:"reservation_ttl" required:"true" min:"1ns" help:"Required and positive: a reservation that never expires burns the key when the caller dies.\nThere is no default because the right value depends on how long the caller's commit path takes."`
	} `config:"directory"`
}

// ConfigSchema 声明 directory.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	directory:
//	  key_prefix: roost:directory   # required, no default
//	  reservation_ttl: 60s          # required, must be positive
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("directory mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Directory
	if err := mods.CheckKeyPrefix("directory", c.KeyPrefix); err != nil {
		return err
	}
	if m.normalize == nil {
		// Checked here rather than at Provide so a misconfigured process
		// fails as early as it can.
		return fmt.Errorf("directory mod: a normalizer is required; it decides which raw keys " +
			"are the same key, which is not a deployment setting")
	}
	m.prefix, m.defaultTTL = c.KeyPrefix, c.ReservationTTL
	return nil
}

// Provide builds the directory and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	state, err := NewRedisState(client, m.prefix, 0)
	if err != nil {
		return fmt.Errorf("directory mod: %w", err)
	}
	dir, err := New(state, Config{
		Normalize: m.normalize, DefaultTTL: m.defaultTTL, Metrics: m.metrics,
	})
	if err != nil {
		return fmt.Errorf("directory mod: %w", err)
	}
	m.directory = dir
	return mods.RegisterAll(r, mods.Capability{Name: m.Name(), Value: dir})
}

// Start implements app.Mod. Nothing to start.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
