package account

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Mod wires an account Service into an app and registers it as a capability.
//
// Three collaborators are required constructor arguments with no defaults,
// each answering a confirmed defect rather than a style preference:
//
//   - Verifier checks an identity with its channel. The implementation this
//     replaces signed a session token for whatever {channel, open_id} arrived,
//     which is complete account takeover — and the missing check looked like a
//     default.
//   - Allocator mints player ids. Its old default was a per-process counter
//     starting at 1, which combined with an upsert meant a restart destroyed
//     existing players.
//   - NameRules validates a display name. There is no universal answer, and a
//     permissive default is an unchecked field.
type Mod struct {
	verifier  IdentityVerifier
	allocator PlayerIDAllocator
	nameRules NameValidator
	metrics   servicemetrics.Reporter

	prefix        string
	sessionSecret string
	sessionTTL    time.Duration
	claimTTL      time.Duration
	service       *Service
}

// NewMod returns an account Mod. All three collaborators are required.
func NewMod(
	verifier IdentityVerifier,
	allocator PlayerIDAllocator,
	nameRules NameValidator,
	reporter servicemetrics.Reporter,
) *Mod {
	return &Mod{verifier: verifier, allocator: allocator, nameRules: nameRules, metrics: reporter}
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// keyPrefixConfig 是 account.key_prefix 的声明：Mod 与 KeyPrefix 共用。
type keyPrefixConfig struct {
	KeyPrefix string `config:"key_prefix" required:"true" example:"roost:{project}:account" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
}

// KeyPrefix 按 Mod 的声明读 account.key_prefix：同一个 Redis 前缀下还有业务自己的键（game-demo 的发货队列、玩家号计数器）的
// 代码用它，与 Mod 读到的是同一个值、同一套检查。
func KeyPrefix(cfg *viper.Viper) (string, error) {
	var settings struct {
		Account keyPrefixConfig `config:"account"`
	}
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", fmt.Errorf("account: %w", err)
	}
	if err := mods.CheckKeyPrefix("account", settings.Account.KeyPrefix); err != nil {
		return "", err
	}
	return settings.Account.KeyPrefix, nil
}

// config 是 account.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	Account struct {
		keyPrefixConfig
		SessionSecret string        `config:"session_secret" required:"true" secret:"true" example:"CHANGE_ME"`
		SessionTTL    time.Duration `config:"session_ttl" default:"30m" min:"1ns" example:"30m"`
		ClaimTTL      time.Duration `config:"claim_ttl" default:"30s" min:"1ns" example:"5m"`
	} `config:"account"`
}

// ConfigSchema 声明 account.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	account:
//	  key_prefix: roost:account   # required, no default
//	  session_secret: "..."       # required, non-empty
//	  session_ttl: 30m            # optional
//	  claim_ttl: 30s              # optional
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("account mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Account
	if err := mods.CheckKeyPrefix("account", c.KeyPrefix); err != nil {
		return err
	}
	missing := []string{}
	if m.verifier == nil {
		missing = append(missing, "identity verifier")
	}
	if m.allocator == nil {
		missing = append(missing, "player id allocator")
	}
	if m.nameRules == nil {
		missing = append(missing, "name validator")
	}
	if len(missing) > 0 {
		return fmt.Errorf("account mod: %v are required and have no defaults; each of them "+
			"was absent in the implementation this replaces, and the absence looked like a default", missing)
	}
	m.prefix, m.sessionSecret, m.sessionTTL, m.claimTTL = c.KeyPrefix, c.SessionSecret, c.SessionTTL, c.ClaimTTL
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	// Collaborators bind first: the ones that implement RegistryBound are
	// exactly the ones that go on to look capabilities up, and their error
	// names the collaborator, which the Redis lookup below cannot.
	if err := bindCollaborators(r, m.verifier, m.allocator, m.nameRules); err != nil {
		return fmt.Errorf("account mod: %w", err)
	}
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	stores, err := NewRedisStores(client, m.prefix, m.claimTTL)
	if err != nil {
		return fmt.Errorf("account mod: %w", err)
	}
	service, err := New(Config{
		Accounts: stores.Accounts, Roles: stores.Roles, Servers: stores.Servers,
		Slots: stores.Slots, Names: stores.Names,
		Verifier: m.verifier, Allocator: m.allocator, NameRules: m.nameRules,
		SessionSecret: m.sessionSecret, SessionTTL: m.sessionTTL, ClaimTTL: m.claimTTL,
		Metrics: m.metrics,
		// Creation and login times are business time; session tokens and
		// operator stamps are system time (D-L3 round 8).
		Now: app.BusinessClock(r).Now, SystemNow: time.Now,
	})
	if err != nil {
		return fmt.Errorf("account mod: %w", err)
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
func bindCollaborators(r *app.Registry, verifier IdentityVerifier, allocator PlayerIDAllocator, rules NameValidator) error {
	collaborators := []struct {
		name  string
		value any
	}{
		{"identity verifier", verifier},
		{"player id allocator", allocator},
		{"name validator", rules},
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

// Start implements app.Mod. Nothing to start.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
