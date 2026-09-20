package session

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
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
	release Releaser
	metrics servicemetrics.Reporter

	prefix     string
	ttl        time.Duration
	requestTTL time.Duration
	service    *Service
}

// NewMod returns a session Mod. release is required.
func NewMod(release Releaser, reporter servicemetrics.Reporter) *Mod {
	return &Mod{release: release, metrics: reporter}
}

// Name implements app.Mod.
// Name implements app.Mod. It returns the generated CapabilityName, so the
// Mod's name and the capability it publishes are one fact rather than two.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// Init reads configuration.
//
//	session:
//	  key_prefix: roost:session   # required, no default
//	  run_ttl: 30m                # optional, defaults to DefaultTTL
//	  request_ttl: 1h             # required; see below
func (m *Mod) Init(cfg *viper.Viper) error {
	if m.release == nil {
		return fmt.Errorf("session mod: a releaser is required; a run that allocates external " +
			"resources and cannot release them is the leak this package prevents, and a default " +
			"that did nothing would make leaking the out-of-the-box behaviour")
	}
	prefix, err := mods.KeyPrefix(cfg, "session")
	if err != nil {
		return err
	}
	ttl, err := mods.Duration(cfg, "session.run_ttl", DefaultTTL)
	if err != nil {
		return err
	}
	// Required, with no default, because the right value is a property of the
	// caller's retry behaviour and getting it wrong is not visible: past the
	// ttl a retried Enter is indistinguishable from a new one, and the caller
	// gets a second run.
	requestTTL, err := mods.RequiredDuration(cfg, "session.request_ttl")
	if err != nil {
		return err
	}
	m.prefix, m.ttl, m.requestTTL = prefix, ttl, requestTTL
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	stores, err := NewRedisStores(client, RedisConfig{
		Prefix: m.prefix, RequestTTL: m.requestTTL,
	})
	if err != nil {
		return fmt.Errorf("session mod: %w", err)
	}
	service, err := New(Config{
		Runs: stores.Runs, Claims: stores.Claims, Requests: stores.Requests,
		Release: m.release, TTL: m.ttl, Metrics: m.metrics,
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
