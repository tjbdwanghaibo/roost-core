package directory

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
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

// Init reads configuration.
//
//	directory:
//	  key_prefix: roost:directory   # required, no default
//	  reservation_ttl: 60s          # required, must be positive
func (m *Mod) Init(cfg *viper.Viper) error {
	if m.normalize == nil {
		// Checked here rather than at Provide so a misconfigured process
		// fails as early as it can.
		return fmt.Errorf("directory mod: a normalizer is required; it decides which raw keys " +
			"are the same key, which is not a deployment setting")
	}
	prefix, err := mods.KeyPrefix(cfg, "directory")
	if err != nil {
		return err
	}
	// Required and positive: a reservation that never expires burns the key
	// when the caller dies, which is the defect this primitive exists to
	// prevent. There is no default because the right value depends on how
	// long the caller's commit path takes.
	ttl, err := mods.RequiredDuration(cfg, "directory.reservation_ttl")
	if err != nil {
		return err
	}
	m.prefix, m.defaultTTL = prefix, ttl
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
