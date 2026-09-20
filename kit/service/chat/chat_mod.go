package chat

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// Mod wires a chat Service into an app and registers it as a capability.
//
// Policy, Bodies and System are required constructor arguments with no
// defaults. The first is the one that matters: a permissive default policy is
// precisely what the client-settable Trusted bool amounted to in the
// implementation this replaces — a way for a caller to be treated as
// privileged without anything having decided that it was.
//
// System turns a transport-level identity into a SystemToken. It has no
// default because minting that token IS the privilege: a default that granted
// it would put back the field this package removed.
type Mod struct {
	policy ChannelPolicy
	bodies *BodyRegistry
	system SystemAuthenticator
	rules  []ChannelRule

	metrics Metrics

	prefix       string
	retentionAge time.Duration
	pruneStatic  []Channel
	pruneDynamic func(context.Context) []ChannelRef
	service      *Service
}

// WithPruneChannels supplies the channels this process prunes that cannot be
// written in configuration — the pair channels of the players currently
// online, say. It is combined with the static `chat.prune_channels` list.
// Refs come from Service.Resolve, which only the owning process can call.
func (m *Mod) WithPruneChannels(provider func(context.Context) []ChannelRef) *Mod {
	m.pruneDynamic = provider
	return m
}

// NewMod returns a chat Mod. policy, bodies and system are required; rules
// may be empty to use DefaultChannelRules.
func NewMod(
	policy ChannelPolicy,
	bodies *BodyRegistry,
	system SystemAuthenticator,
	rules []ChannelRule,
	reporter Metrics,
) *Mod {
	return &Mod{
		policy: policy, bodies: bodies, system: system,
		rules: append([]ChannelRule(nil), rules...), metrics: reporter,
	}
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// Init reads configuration.
//
//	chat:
//	  key_prefix: roost:chat   # required, no default
//	  retention_age: 168h      # optional; zero means retain by count only
func (m *Mod) Init(cfg *viper.Viper) error {
	missing := []string{}
	if m.policy == nil {
		missing = append(missing, "channel policy")
	}
	if m.bodies == nil {
		missing = append(missing, "body registry")
	}
	if m.system == nil {
		missing = append(missing, "system authenticator")
	}
	if len(missing) > 0 {
		return fmt.Errorf("chat mod: %v are required and have no defaults; a permissive policy "+
			"is what the client-settable Trusted bool amounted to", missing)
	}
	prefix, err := mods.KeyPrefix(cfg, "chat")
	if err != nil {
		return err
	}
	retention, err := mods.Duration(cfg, "chat.retention_age", 0)
	if err != nil {
		return err
	}
	static, err := parsePruneChannels(cfg.GetStringSlice("chat.prune_channels"))
	if err != nil {
		return err
	}
	m.prefix, m.retentionAge, m.pruneStatic = prefix, retention, static
	return nil
}

// parsePruneChannels reads `chat.prune_channels`, a list of "kind:target"
// entries naming shared-scope channels — "world:1", "group:42". Pair-scoped
// kinds cannot be listed here because they need a participant; they come
// through WithPruneChannels.
func parsePruneChannels(entries []string) ([]Channel, error) {
	channels := make([]Channel, 0, len(entries))
	for _, entry := range entries {
		kind, target, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || kind == "" || target == "" {
			return nil, fmt.Errorf("chat mod: chat.prune_channels entry %q must be kind:target", entry)
		}
		id, err := strconv.ParseInt(target, 10, 64)
		if err != nil || id < 0 {
			return nil, fmt.Errorf("chat mod: chat.prune_channels entry %q has a non-numeric target", entry)
		}
		channels = append(channels, Channel{Kind: ChannelKind(kind), Target: id})
	}
	return channels, nil
}

// resolvePruneChannels turns the configured channels into refs against the
// store. It fails closed: an entry that does not resolve — an unknown kind, a
// pair-scoped kind, a missing target — stops the process at startup rather
// than being skipped on every tick.
func resolvePruneChannels(store Store, channels []Channel) ([]ChannelRef, error) {
	refs := make([]ChannelRef, 0, len(channels))
	for _, ch := range channels {
		ref, err := store.Resolve(ch, 0)
		if err != nil {
			return nil, fmt.Errorf("chat mod: chat.prune_channels %s:%d: %w", ch.Kind, ch.Target, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	store, err := NewRedisStore(client, m.prefix, Config{
		Policy: m.policy, Bodies: m.bodies, Rules: m.rules,
		RetentionAge: m.retentionAge, Metrics: m.metrics,
	})
	if err != nil {
		return fmt.Errorf("chat mod: %w", err)
	}
	static, err := resolvePruneChannels(store, m.pruneStatic)
	if err != nil {
		return err
	}
	var prune func(context.Context) []ChannelRef
	if len(static) > 0 || m.pruneDynamic != nil {
		dynamic := m.pruneDynamic
		prune = func(ctx context.Context) []ChannelRef {
			refs := append([]ChannelRef(nil), static...)
			if dynamic != nil {
				refs = append(refs, dynamic(ctx)...)
			}
			return refs
		}
	}
	service, err := NewService(ServiceConfig{Store: store, System: m.system, PruneChannels: prune})
	if err != nil {
		return fmt.Errorf("chat mod: %w", err)
	}
	m.service = service
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface consumers look up, and the owner-only name the
	// Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(service)...)
}

// Start implements app.Mod. Nothing to start.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
