package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

func modConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("mail.key_prefix", "roost:mail")
	cfg.Set("mail.send_ttl", 24*time.Hour)
	return cfg
}

// The send ttl has no default. It must exceed the longest client retry
// horizon: past it a retried send is indistinguishable from a new one and the
// recipient gets the mail twice. That horizon belongs to the caller's
// transport, so this package cannot pick it.
func TestModRequiresASendTTL(t *testing.T) {
	cfg := viper.New()
	cfg.Set("mail.key_prefix", "roost:mail")
	if err := NewMod(nil, nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a missing send ttl")
	}
	cfg.Set("mail.send_ttl", time.Duration(0))
	if err := NewMod(nil, nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a zero send ttl")
	}
}

// A nil broadcast deliverer is allowed, and it is the one optional
// collaborator in this repository that is safe to omit — because omitting it
// weakens nothing: Send refuses AudienceBroadcast outright rather than
// accepting it and delivering to nobody. This asserts that, so "optional"
// stays a property of the code rather than of a comment.
func TestAMissingBroadcastDelivererIsAllowedAndRefusesBroadcasts(t *testing.T) {
	mod := NewMod(nil, nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatalf("Init refused a mod with no broadcast deliverer: %v", err)
	}

	// And the refusal really happens, at the service level.
	h := newHarness(t, func(cfg *Config) { cfg.Broadcast = nil })
	_, err := h.service.Send(context.Background(), SendRequest{
		Audience: AudienceBroadcast, Subject: "maintenance",
		ExpiresInSeconds: 3600, RequestID: "send-1",
	})
	if !errors.Is(err, ErrAudienceInvalid) {
		t.Fatalf("a broadcast with no deliverer produced %v, want ErrAudienceInvalid; "+
			"accepting it would be a mail nobody receives reported as a successful send", err)
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(nil, nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModMail {
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
