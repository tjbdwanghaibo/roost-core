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

// Init reads configuration.
//
//	account:
//	  key_prefix: roost:account   # required, no default
//	  session_secret: "..."       # required, non-empty
//	  session_ttl: 30m            # optional
//	  claim_ttl: 30s              # optional
func (m *Mod) Init(cfg *viper.Viper) error {
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
	prefix, err := mods.KeyPrefix(cfg, "account")
	if err != nil {
		return err
	}
	sessionSecret, err := mods.Secret(cfg, "account.session_secret")
	if err != nil {
		return err
	}
	sessionTTL, err := mods.Duration(cfg, "account.session_ttl", DefaultSessionTTL)
	if err != nil {
		return err
	}
	claimTTL, err := mods.Duration(cfg, "account.claim_ttl", DefaultClaimTTL)
	if err != nil {
		return err
	}
	m.prefix, m.sessionSecret, m.sessionTTL, m.claimTTL = prefix, sessionSecret, sessionTTL, claimTTL
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
