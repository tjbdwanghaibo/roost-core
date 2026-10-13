package account

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	domain "github.com/tjbdwanghaibo/roost-core/service/account"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func modConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("account.key_prefix", "roost:account")
	cfg.Set("account.session_secret", "s3cret")
	return cfg
}

// Each of these three was ABSENT in the implementation this replaces, and the
// absence looked like a default. A Mod that supplied one would put the
// vulnerability back — so the Mod must refuse to start without them, and that
// refusal is the only load-bearing thing in the file.
func TestModRefusesWithoutItsRequiredCollaborators(t *testing.T) {
	verifier := acceptingVerifier()
	allocator := &durableAllocator{}
	rules := simpleNameRules()

	cases := []struct {
		name string
		mod  *Mod
		want string
	}{
		{"no verifier", NewMod(nil, allocator, rules, nil), "identity verifier"},
		{"no allocator", NewMod(verifier, nil, rules, nil), "player id allocator"},
		{"no name rules", NewMod(verifier, allocator, nil, nil), "name validator"},
		{"none at all", NewMod(nil, nil, nil, nil), "identity verifier"},
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

// The session secret is refused when empty, at Init. A secret checked at call
// time turns an unset value into a silent outage.
func TestModRefusesAnEmptySessionSecret(t *testing.T) {
	mod := NewMod(acceptingVerifier(), &durableAllocator{}, simpleNameRules(), nil)
	cfg := viper.New()
	cfg.Set("account.key_prefix", "roost:account")
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a missing session secret")
	}
	cfg.Set("account.session_secret", "  ")
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a whitespace-only session secret")
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(acceptingVerifier(), &durableAllocator{}, simpleNameRules(), nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModAccount {
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
	err := mod.Provide(app.NewRegistry(viper.New()))
	if err == nil {
		t.Fatal("Provide succeeded with no Redis capability")
	}
	if !strings.Contains(err.Error(), string(mods.ModRedis)) {
		t.Fatalf("the error does not name the missing capability: %v", err)
	}
	// And Stop on a mod that never provided must not panic: a process that
	// failed to start still runs its shutdown path.
	mod.Stop()
	if err := mod.Start(); err != nil {
		t.Fatalf("Start on a mod that never provided returned %v", err)
	}
}

// 实现了 RegistryBound 的 collaborator 在 Mod.Provide 里拿到 account 服务进程的 registry，且拿到的时刻早于 Mod 自己去查 Redis；
// Bind 返回错误则 Provide 失败并点名是哪一个 collaborator（PlayerIDAllocator 这类需要持久共享计数器的 collaborator 由此拿到能力）。

type boundAllocator struct {
	durableAllocator
	got  *app.Registry
	fail error
}

func (a *boundAllocator) BindRegistry(r *app.Registry) error {
	a.got = r
	return a.fail
}

func TestProvideBindsRegistryAwareCollaborators(t *testing.T) {
	allocator := &boundAllocator{}
	mod := NewMod(acceptingVerifier(), allocator, simpleNameRules(), nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(viper.New())
	// Provide fails afterwards for want of Redis; binding must already have
	// happened, because a bound collaborator is what looks Redis up itself.
	_ = mod.Provide(registry)
	if allocator.got != registry {
		t.Fatalf("the allocator was not handed the registry (got %p, want %p)", allocator.got, registry)
	}
}

func TestProvideReportsACollaboratorThatRefusesToBind(t *testing.T) {
	allocator := &boundAllocator{fail: errors.New("counter store missing")}
	mod := NewMod(acceptingVerifier(), allocator, simpleNameRules(), nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	err := mod.Provide(app.NewRegistry(viper.New()))
	if err == nil {
		t.Fatal("Provide succeeded although the allocator refused to bind")
	}
	for _, want := range []string{"player id allocator", "counter store missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not carry %q", err, want)
		}
	}
}

// The generated RPC glue is the same template in every roost-service package;
// its three refusals are pinned once, here. A process that holds only a
// client to Accounts must not be allowed to serve Accounts (it would forward
// every request to itself), a process with neither must be told what to add,
// and a negative call timeout is a mistake, not a default.
func TestGeneratedServerInitRefusesAClientOnlyProcessAndNamesTheMissingMod(t *testing.T) {
	empty := app.NewRegistry(viper.New())
	err := NewServer().Init(empty)
	if err == nil || !strings.Contains(err.Error(), `capability "`+string(domain.LocalCapabilityName)+`" not found; add account.NewMod()`) {
		t.Fatalf("Init on an empty registry = %v", err)
	}

	clientOnly := app.NewRegistry(viper.New())
	if err := clientOnly.Register(domain.CapabilityName, &domain.Service{}); err != nil {
		t.Fatal(err)
	}
	err = NewServer().Init(clientOnly)
	if err == nil || !strings.Contains(err.Error(), "is published but") || !strings.Contains(err.Error(), "forward every request to itself") {
		t.Fatalf("Init on a client-only registry = %v", err)
	}
}

func TestGeneratedClientModRefusesANegativeCallTimeout(t *testing.T) {
	cfg := viper.New()
	cfg.Set("account.call_timeout", -time.Second)
	err := NewClientMod().Init(cfg)
	if err == nil || !strings.Contains(err.Error(), "account.call_timeout must not be negative, got -1s") {
		t.Fatalf("Init with a negative timeout = %v", err)
	}
	cfg.Set("account.call_timeout", 0)
	if err := NewClientMod().Init(cfg); err != nil {
		t.Fatalf("zero means default and must be accepted: %v", err)
	}
}
