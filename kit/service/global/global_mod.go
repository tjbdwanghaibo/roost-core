package global

import (
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// Mod wires the routing and lease service into an app and registers it as a
// capability.
//
// It used to wire two services and publish two capabilities: routing/leases
// and cross-server activity coordination lived in one package. They shared no
// state — routing answers "where does this game belong and is it alive",
// activity coordination answers "has every game reached the phase yet" — and
// app.Service is one per process, so they were always two deployments. They
// are now two packages; the activity Mod is activity.NewMod.
//
// The Mod needs no policy collaborator: every decision here is a
// compare-and-set against state, not a judgement about a caller.
type Mod struct {
	metrics servicemetrics.Reporter

	prefix   string
	leaseTTL time.Duration

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

// Init reads configuration.
//
//	global:
//	  key_prefix: roost:global   # required, no default
//	  lease_ttl: 30s             # optional
//
// The activity settings that used to live under this key — reservation_ttl,
// grace_window, dispatch_attempts, dispatch_backoff — moved with the service,
// to activity.Mod's own section.
func (m *Mod) Init(cfg *viper.Viper) error {
	prefix, err := mods.KeyPrefix(cfg, "global")
	if err != nil {
		return err
	}
	leaseTTL, err := mods.Duration(cfg, "global.lease_ttl", DefaultLeaseTTL)
	if err != nil {
		return err
	}
	m.prefix, m.leaseTTL = prefix, leaseTTL
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
	service, err := New(Config{
		Routes: stores.Routes, Leases: stores.Leases,
		LeaseTTL: m.leaseTTL, Metrics: m.metrics,
	})
	if err != nil {
		return err
	}
	m.service = service
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface consumers look up, and the owner-only name the
	// Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(service)...)
}

// Start implements app.Mod.
//
// Nothing is started here. Lapsed leases are reclaimed by the Server's run
// hook, which runs in the process that OWNS this service — a Mod cannot own
// that loop, because a process that merely holds a global client would then be
// reclaiming leases it does not own.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
