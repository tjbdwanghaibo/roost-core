package platform

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// Mod wires a platform Service into an app and registers it as a capability.
//
// Three collaborators are required constructor arguments with no defaults, and
// this is the file where that matters most in the whole repository. The
// implementation this replaces signed a session token for whatever player id
// arrived in the request body — there was no verifier, and the ABSENCE looked
// exactly like a default. So:
//
//   - Verifier checks a credential with its channel. A permissive default is
//     total account takeover.
//   - Players maps a verified identity to a player id. A default would either
//     invent ids or read them from the request.
//   - Deliver grants the goods for a paid order. A default that did nothing
//     would take money and deliver nothing, silently.
//
// The two secrets come from configuration and are refused when empty, at Init.
type Mod struct {
	verifier Verifier
	players  PlayerResolver
	deliver  Deliverer
	pending  PendingOrders
	metrics  servicemetrics.Reporter

	prefix        string
	sessionSecret string
	paymentSecret string
	sessionTTL    time.Duration
	attempts      int
	backoff       time.Duration
	service       *Service
}

// NewMod returns a platform Mod. All three collaborators are required.
func NewMod(verifier Verifier, players PlayerResolver, deliver Deliverer, reporter servicemetrics.Reporter) *Mod {
	return &Mod{verifier: verifier, players: players, deliver: deliver, metrics: reporter}
}

// WithPendingOrders gives the Server's background loop a source of
// paid-but-undelivered orders to retry. Optional and separate from NewMod on
// purpose: the three constructor collaborators have no safe default, while
// this one has a defensible "none" — a deployment whose channel re-delivers
// callbacks does not need it. What it must not be is invisible, so a Server
// without it says so at start (RR-20260917-04).
func (m *Mod) WithPendingOrders(pending PendingOrders) *Mod {
	if m != nil {
		m.pending = pending
	}
	return m
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// Init reads configuration.
//
//	platform:
//	  key_prefix: roost:platform   # required, no default
//	  session_secret: "..."        # required, non-empty
//	  payment_secret: "..."        # required, non-empty
//	  session_ttl: 30m             # optional
//	  delivery_attempts: 8         # optional
//	  delivery_backoff: 5s         # optional
func (m *Mod) Init(cfg *viper.Viper) error {
	missing := []string{}
	if m.verifier == nil {
		missing = append(missing, "identity verifier")
	}
	if m.players == nil {
		missing = append(missing, "player resolver")
	}
	if m.deliver == nil {
		missing = append(missing, "recharge deliverer")
	}
	if len(missing) > 0 {
		return fmt.Errorf("platform mod: %v are required and have no defaults; a permissive "+
			"verifier is account takeover and a no-op deliverer takes money without delivering", missing)
	}
	prefix, err := mods.KeyPrefix(cfg, "platform")
	if err != nil {
		return err
	}
	// Refused at Init, not at call time. An unset payment secret in the
	// implementation this replaces turned every provider callback into an
	// invalid-signature refusal: a silent outage that looked like an attack.
	sessionSecret, err := mods.Secret(cfg, "platform.session_secret")
	if err != nil {
		return err
	}
	paymentSecret, err := mods.Secret(cfg, "platform.payment_secret")
	if err != nil {
		return err
	}
	sessionTTL, err := mods.Duration(cfg, "platform.session_ttl", DefaultSessionTTL)
	if err != nil {
		return err
	}
	backoff, err := mods.Duration(cfg, "platform.delivery_backoff", DefaultDeliveryBackoff)
	if err != nil {
		return err
	}
	attempts := MaxDeliveryAttempts
	if cfg.IsSet("platform.delivery_attempts") {
		attempts = cfg.GetInt("platform.delivery_attempts")
		if attempts <= 0 {
			return fmt.Errorf("platform mod: platform.delivery_attempts must be positive, got %d", attempts)
		}
	}
	m.prefix, m.sessionSecret, m.paymentSecret = prefix, sessionSecret, paymentSecret
	m.sessionTTL, m.attempts, m.backoff = sessionTTL, attempts, backoff
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	// Collaborators bind first: the ones that implement RegistryBound are
	// exactly the ones that go on to look capabilities up, and their error
	// names the collaborator, which the Redis lookup below cannot.
	if err := bindCollaborators(r, m.verifier, m.players, m.deliver, m.pending); err != nil {
		return fmt.Errorf("platform mod: %w", err)
	}
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	orders, err := NewRedisOrders(client, m.prefix)
	if err != nil {
		return fmt.Errorf("platform mod: %w", err)
	}
	service, err := New(Config{
		Orders: orders, Deliver: m.deliver, Verifier: m.verifier, Players: m.players, Pending: m.pending,
		SessionSecret: m.sessionSecret, SessionTTL: m.sessionTTL,
		PaymentSecret:    m.paymentSecret,
		DeliveryAttempts: m.attempts, DeliveryBackoff: m.backoff,
		Metrics: m.metrics,
	})
	if err != nil {
		return fmt.Errorf("platform mod: %w", err)
	}
	m.service = service
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface consumers look up, and the owner-only name the
	// Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(service)...)
}

// bindCollaborators hands the registry to every collaborator that asked for
// one (RegistryBound). The names match the ones Init's "required" error uses,
// so an operator reading logs sees one vocabulary.
func bindCollaborators(r *app.Registry, verifier Verifier, players PlayerResolver, deliver Deliverer, pending PendingOrders) error {
	collaborators := []struct {
		name  string
		value any
	}{
		{"identity verifier", verifier},
		{"player resolver", players},
		{"recharge deliverer", deliver},
		{"pending order index", pending},
	}
	for _, collaborator := range collaborators {
		bound, ok := collaborator.value.(RegistryBound)
		if !ok {
			continue
		}
		if err := bound.BindRegistry(r); err != nil {
			return fmt.Errorf("bind %s: %w", collaborator.name, err)
		}
	}
	return nil
}

// Start implements app.Mod.
//
// Nothing is started here. Orders whose delivery failed are retried by the
// Server's run hook, which runs in the process that OWNS this service — a Mod
// cannot own that loop, because a process that merely holds a platform client
// would then be retrying deliveries it does not own.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
