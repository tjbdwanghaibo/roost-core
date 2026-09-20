package split

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	kitredis "github.com/tjbdwanghaibo/roost-core/redis/driver"
	kitmods "github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/tjbdwanghaibo/roost-kit/service/mail"
)

// The same consumer code resolves in both deployments.
//
// This is the claim the whole design exists to support, and an example that
// only compiled would not establish it: compiling proves the lookup TYPECHECKS
// in both, not that it RESOLVES in both. So each side is bootstrapped and the
// consumer is constructed against it.
func TestTheConsumerResolvesInBothDeployments(t *testing.T) {
	t.Run("owner process", func(t *testing.T) {
		registry := ownerRegistry(t)
		if _, err := NewRewardFlow(registry); err != nil {
			t.Fatalf("the consumer could not be built in the owning process: %v", err)
		}
	})
	t.Run("caller process", func(t *testing.T) {
		registry := callerRegistry(t)
		if _, err := NewRewardFlow(registry); err != nil {
			t.Fatalf("the consumer could not be built in a calling process: %v", err)
		}
	})
}

// A consumer that bound to the local concrete type would work in one
// deployment and fail in the other. It must fail in BOTH, so the mistake is
// caught on the first deployment rather than on the day mail is split out.
func TestBindingToTheConcreteTypeFailsInBothDeployments(t *testing.T) {
	for name, registry := range map[string]*app.Registry{
		"owner process":  ownerRegistry(t),
		"caller process": callerRegistry(t),
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := app.Lookup[*mail.Service](registry, mail.CapabilityName); ok {
				t.Fatal("the capability resolves as *mail.Service; a consumer can bind to the " +
					"local type, and that consumer breaks when mail moves into its own process")
			}
			if _, ok := app.Lookup[*mail.BusClient](registry, mail.CapabilityName); ok {
				t.Fatal("the capability resolves as *mail.BusClient; a consumer can bind to the " +
					"remote type, and that consumer breaks in the process that owns mail")
			}
		})
	}
}

// Only the owning process publishes the owner-only name, which is how the
// Server knows it holds the implementation rather than a client to it.
func TestOnlyTheOwningProcessPublishesTheLocalCapability(t *testing.T) {
	if _, ok := app.Lookup[mail.Mail](ownerRegistry(t), mail.LocalCapabilityName); !ok {
		t.Fatal("the owning process does not publish the owner-only capability; its Server " +
			"could not tell itself apart from a caller")
	}
	if _, ok := app.Lookup[mail.Mail](callerRegistry(t), mail.LocalCapabilityName); ok {
		t.Fatal("a calling process publishes the owner-only capability; a Server started there " +
			"would forward every request to itself")
	}
}

// Registering both Mods in one process fails, and the error names the
// capability rather than leaving someone to guess.
func TestAProcessCannotOwnAndCallTheSameService(t *testing.T) {
	registry := ownerRegistry(t)
	// The bus too, so the client Mod gets far enough to try to register.
	// Without it the client fails on a missing bus and the conflict this test
	// is about never happens — which is how an earlier version of this test
	// passed for the wrong reason.
	if err := registry.Register(kitmods.ModBus, stubBus{}); err != nil {
		t.Fatal(err)
	}
	client := mail.NewClientMod()
	if err := client.Init(config()); err != nil {
		t.Fatal(err)
	}
	err := client.Provide(registry)
	if err == nil {
		t.Fatal("a process registered both the owning Mod and the client Mod")
	}
	if !strings.Contains(err.Error(), string(mail.CapabilityName)) {
		t.Fatalf("the error does not name the conflicting capability: %v", err)
	}
}

// --- bootstrapping the two sides ---

// config is the configuration both sides read. The owner needs the store
// settings; the caller ignores them, which is why a caller cannot be
// misconfigured with a prefix that disagrees with the owner's.
func config() *viper.Viper {
	cfg := viper.New()
	cfg.Set("mail.key_prefix", "roost:example:mail")
	cfg.Set("mail.send_ttl", "24h")
	return cfg
}

// ownerRegistry runs the owning process's Mods, the way app.run does.
//
// Driven by hand rather than through app.App because Execute runs a whole
// cobra command; what is under test here is the Mods' contract, and roost-core
// tests App's own plumbing.
func ownerRegistry(t *testing.T) *app.Registry {
	t.Helper()
	cfg := config()
	registry := app.NewRegistry(cfg)
	// The REAL kit client, pointed at an address nothing listens on.
	//
	// go-redis connects lazily, so constructing it issues no traffic, and the
	// owning Mod only builds stores from it — no test here sends a command.
	// Using the real type rather than a forty-method stub keeps this example
	// readable, which is the only reason an example exists.
	client, err := kitredis.NewClient(fredis.DefaultConfig("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := registry.Register(kitmods.ModRedis, client); err != nil {
		t.Fatal(err)
	}
	mod := mail.NewMod(nil, nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func callerRegistry(t *testing.T) *app.Registry {
	t.Helper()
	cfg := config()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(kitmods.ModBus, stubBus{}); err != nil {
		t.Fatal(err)
	}
	mod := mail.NewClientMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	return registry
}
