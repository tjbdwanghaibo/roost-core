package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	domain "github.com/tjbdwanghaibo/roost-core/service/chat"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func modConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("chat.key_prefix", "roost:chat")
	return cfg
}

// A permissive default policy is precisely what the client-settable Trusted
// bool amounted to: a way for a caller to be treated as privileged without
// anything having decided that it was. And a default SystemAuthenticator
// would mint the privilege token itself, putting back the field this package
// removed.
func TestModRefusesWithoutItsRequiredCollaborators(t *testing.T) {
	policy := allowAllPolicy{}
	registry := testRegistry(t)
	system := grantingAuth()

	cases := []struct {
		name string
		mod  *Mod
		want string
	}{
		{"no policy", NewMod(nil, registry, system, nil, nil), "channel policy"},
		{"no body registry", NewMod(policy, nil, system, nil, nil), "body registry"},
		{"no system authenticator", NewMod(policy, registry, nil, nil, nil), "system authenticator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mod.Init(modConfig())
			if err == nil {
				t.Fatalf("Init accepted a mod with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error does not name what is missing (%q): %v", tc.want, err)
			}
		})
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(allowAllPolicy{}, testRegistry(t), grantingAuth(), nil, nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModChat {
		t.Fatalf("the mod is named %q", got)
	}
	var asAny any = mod
	provider, ok := asAny.(app.ModDependencyProvider)
	if !ok {
		t.Fatal("the Mod does not declare its dependencies")
	}
	if len(provider.DependsOn()) == 0 || provider.DependsOn()[0] != mods.ModRedis {
		t.Fatalf("DependsOn = %v, want redis", provider.DependsOn())
	}
	if err := mod.Provide(app.NewRegistry(viper.New())); err == nil {
		t.Fatal("Provide succeeded with no Redis capability")
	} else if !strings.Contains(err.Error(), string(mods.ModRedis)) {
		t.Fatalf("the error does not name the missing capability: %v", err)
	}
	mod.Stop()
	if err := mod.Start(); err != nil {
		t.Fatalf("Start returned %v", err)
	}
}

// The rules slice is copied at construction, so a caller mutating its own
// slice afterwards cannot change what the service enforces.
func TestModCopiesTheRulesItWasGiven(t *testing.T) {
	rules := []domain.ChannelRule{{Kind: domain.ChannelWorld, RequiresTarget: true, Scope: domain.ScopeShared, Retain: 4}}
	mod := NewMod(allowAllPolicy{}, testRegistry(t), grantingAuth(), rules, nil)
	rules[0].Retain = 999
	rules[0].SystemOnly = true
	if mod.rules[0].Retain != 4 {
		t.Fatalf("the mod's rule changed with the caller's slice: retain = %d", mod.rules[0].Retain)
	}
	if mod.rules[0].SystemOnly {
		t.Fatal("the mod's rule became system-only because the caller mutated its slice")
	}
}

// 仅解析频道，不允许发起 Redis 请求；嵌入的 nil 方法在误用时直接失败。
type unusedRedis struct{ fredis.IRedis }

// Configuration names shared channels as kind:target. A malformed entry, or
// a pair-scoped kind that needs a participant, stops the process at startup
// rather than being skipped on every tick.
func TestPruneChannelsConfigurationFailsClosed(t *testing.T) {
	store, err := domain.NewRedisStore(&unusedRedis{}, "test:chat", domain.Config{Policy: allowAllPolicy{}, Bodies: domain.NewBodyRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"world", "world:", ":1", "world:abc", "world:-1"} {
		if _, err := parsePruneChannels([]string{bad}); err == nil {
			t.Errorf("entry %q was accepted", bad)
		}
	}
	channels, err := parsePruneChannels([]string{"world:1", " group:42 "})
	if err != nil || len(channels) != 2 || channels[1] != (domain.Channel{Kind: domain.ChannelGroup, Target: 42}) {
		t.Fatalf("parsed %+v err=%v", channels, err)
	}
	if _, err := resolvePruneChannels(store, []domain.Channel{{Kind: domain.ChannelPrivate, Target: 7}}); !errors.Is(err, domain.ErrChannelInvalid) {
		t.Fatalf("a pair-scoped kind in chat.prune_channels was resolved: %v", err)
	}
	refs, err := resolvePruneChannels(store, channels)
	if err != nil || len(refs) != 2 || refs[0].Key() != "world:1" {
		t.Fatalf("refs=%v err=%v", refs, err)
	}

	cfg := modConfig()
	cfg.Set("chat.prune_channels", []string{"world:oops"})
	mod := NewMod(allowAllPolicy{}, domain.NewBodyRegistry(), domain.SystemAuthenticatorFunc(func(context.Context) (domain.SystemToken, error) {
		return domain.SystemToken{}, fmt.Errorf("no system path")
	}), nil, nil)
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a malformed chat.prune_channels entry")
	}
}
