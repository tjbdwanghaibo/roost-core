package platform

import domain "github.com/tjbdwanghaibo/roost-core/service/platform"

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
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
	verifier          domain.Verifier
	players           domain.PlayerResolver
	deliver           domain.Deliverer
	pending           domain.PendingOrders
	pendingConfigured bool
	metrics           servicemetrics.Reporter

	prefix        string
	sessionSecret string
	paymentSecret string
	sessionTTL    time.Duration
	attempts      int
	backoff       time.Duration
	service       *domain.Service
}

// NewMod returns a platform Mod. All three collaborators are required.
func NewMod(verifier domain.Verifier, players domain.PlayerResolver, deliver domain.Deliverer, reporter servicemetrics.Reporter) *Mod {
	return &Mod{verifier: verifier, players: players, deliver: deliver, metrics: reporter}
}

// WithPendingOrders gives the Server's background loop a source of
// paid-but-undelivered orders to retry. Optional and separate from NewMod on
// purpose: the three constructor collaborators have no safe default, while
// this one has a defensible "none" — a deployment whose channel re-delivers
// callbacks does not need it. What it must not be is invisible, so a Server
// without it says so at start (RR-20260917-04).
func (m *Mod) WithPendingOrders(pending domain.PendingOrders) *Mod {
	if m != nil {
		m.pending = pending
		m.pendingConfigured = true
	}
	return m
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return domain.CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// keyPrefixConfig 是 platform.key_prefix 的声明：Mod 与 KeyPrefix 共用。
type keyPrefixConfig struct {
	KeyPrefix string `config:"key_prefix" required:"true" example:"roost:{project}:platform" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
}

// KeyPrefix 按 Mod 的声明读 platform.key_prefix：同一个 Redis 前缀下还有业务自己的键（game-demo 的发货队列、玩家号计数器）的
// 代码用它，与 Mod 读到的是同一个值、同一套检查。
func KeyPrefix(cfg *viper.Viper) (string, error) {
	var settings struct {
		Platform keyPrefixConfig `config:"platform"`
	}
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", fmt.Errorf("platform: %w", err)
	}
	if err := mods.CheckKeyPrefix("platform", settings.Platform.KeyPrefix); err != nil {
		return "", err
	}
	return settings.Platform.KeyPrefix, nil
}

// config 是 platform.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	kitredis.ClusterConfig
	Platform struct {
		keyPrefixConfig
		SessionSecret    string        `config:"session_secret" required:"true" secret:"true" example:"CHANGE_ME" help:"session_secret and payment_secret are refused when empty at Init (an unset payment secret turns\nevery provider callback into an invalid-signature refusal), so they are emitted as CHANGE_ME."`
		PaymentSecret    string        `config:"payment_secret" required:"true" secret:"true" example:"CHANGE_ME"`
		SessionTTL       time.Duration `config:"session_ttl" default:"30m" min:"1ns" example:"30m"`
		DeliveryAttempts int           `config:"delivery_attempts" default:"8" min:"1" example:"8"`
		DeliveryBackoff  time.Duration `config:"delivery_backoff" default:"5s" min:"1ns"`
	} `config:"platform"`
}

// ConfigSchema 声明 platform.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

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
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("platform mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Platform
	if err := mods.CheckKeyPrefix("platform", c.KeyPrefix); err != nil {
		return err
	}
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
	// The order and its index entry are written by one script, and one script
	// can only be atomic across two keys if both keys hash to the same slot.
	// On a single Redis that is free; on a cluster it requires a hash tag in
	// the prefix — `{...}` around the part the order keys and the index share.
	// Refused here rather than discovered as a CROSSSLOT error on the first
	// callback, or, worse, as an index that is only usually right
	// (RR-20260919-04).
	if err := mods.ValidateClusterKeyPrefix(settings.ClusterAddrs, "platform", c.KeyPrefix); err != nil {
		return err
	}
	m.prefix, m.sessionSecret, m.paymentSecret = c.KeyPrefix, c.SessionSecret, c.PaymentSecret
	m.sessionTTL, m.attempts, m.backoff = c.SessionTTL, c.DeliveryAttempts, c.DeliveryBackoff
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
	orders, err := domain.NewRedisOrders(client, m.prefix)
	if err != nil {
		return fmt.Errorf("platform mod: %w", err)
	}
	// The store keeps its own pending index, in the same write as the order
	// (RR-20260919-04), so background retry is ON by default and a deployment
	// no longer has to supply an index to get recovery. WithPendingOrders
	// still wins: a deployment whose orders live somewhere else, or that wants
	// retry off, says so explicitly.
	pending := m.pending
	if !m.pendingConfigured {
		pending = orders
	}
	service, err := domain.New(domain.Config{
		Orders: orders, Deliver: m.deliver, Verifier: m.verifier, Players: m.players, Pending: pending,
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
func bindCollaborators(r *app.Registry, verifier domain.Verifier, players domain.PlayerResolver, deliver domain.Deliverer, pending domain.PendingOrders) error {
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
