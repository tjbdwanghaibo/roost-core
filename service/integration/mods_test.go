//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/bus"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/security"

	"github.com/tjbdwanghaibo/roost-kit/mods"
	"github.com/tjbdwanghaibo/roost-kit/service/account"
	"github.com/tjbdwanghaibo/roost-kit/service/chat"
	"github.com/tjbdwanghaibo/roost-kit/service/directory"
	"github.com/tjbdwanghaibo/roost-kit/service/global"
	"github.com/tjbdwanghaibo/roost-kit/service/global/activity"
	"github.com/tjbdwanghaibo/roost-kit/service/mail"
	"github.com/tjbdwanghaibo/roost-kit/service/match"
	"github.com/tjbdwanghaibo/roost-kit/service/platform"
	"github.com/tjbdwanghaibo/roost-kit/service/rank"
	"github.com/tjbdwanghaibo/roost-kit/service/session"
)

// This is the end-to-end proof for the deployment surface: every service Mod
// is taken through the lifecycle an app puts it through — Init from
// configuration, Provide against a registry holding a real Redis — and every
// capability it claims to publish is then looked up and USED.
//
// Driving the lifecycle by hand rather than through app.App is deliberate.
// App.run is private and Execute runs a whole cobra command, so a test that
// went through it would be testing App's plumbing, which roost-core already
// tests. What is untested until here is the Mods' own contract: that Init
// accepts a realistic configuration, that Provide finds what it needs, and
// that what lands in the registry is the type a caller will assert on.

// modConfig is one configuration for all nine services, shaped the way a
// deployment's roost.yaml would be — including the required values that have
// no defaults, since a config missing any of them is supposed to fail.
func modConfig(root string) *viper.Viper {
	cfg := viper.New()
	for service, prefix := range map[string]string{
		"account": ":account", "activity": ":activity", "chat": ":chat",
		"directory": ":directory", "global": ":global", "mail": ":mail",
		"match": ":match", "platform": ":platform", "rank": ":rank",
		"session": ":session",
	} {
		cfg.Set(service+".key_prefix", root+prefix)
	}
	cfg.Set("account.session_secret", "itest-account-secret")
	cfg.Set("directory.reservation_ttl", time.Minute)
	cfg.Set("activity.reservation_ttl", 30*time.Minute)
	cfg.Set("mail.send_ttl", 24*time.Hour)
	cfg.Set("platform.session_secret", "itest-platform-session")
	cfg.Set("platform.payment_secret", "itest-platform-payment")
	cfg.Set("session.request_ttl", time.Hour)
	return cfg
}

// serviceMods builds every Mod with the policy collaborators a business repo
// would supply. Every one of these is required with no default, which is the
// point: this list is what a deployment must decide, written out.
func serviceMods(t *testing.T) []app.Mod {
	t.Helper()
	var nextPlayer int64 = 7000
	registry := chat.NewBodyRegistry()
	if err := registry.Register("text", chat.BodySpec{Validator: chat.TextValidator(200)}); err != nil {
		t.Fatal(err)
	}
	return []app.Mod{
		account.NewMod(
			account.VerifierFunc(func(_ context.Context, id account.Identity) (account.Verified, error) {
				return account.Verified{Channel: id.Channel, OpenID: id.OpenID}, nil
			}),
			account.AllocatorFunc(func(context.Context, int32) (int64, error) {
				nextPlayer++
				return nextPlayer, nil
			}),
			account.NameValidatorFunc(func(string) error { return nil }),
			nil,
		),
		chat.NewMod(permissivePolicy{}, registry,
			chat.SystemAuthenticatorFunc(func(context.Context) (chat.SystemToken, error) {
				return chat.GrantSystem(), nil
			}), nil, nil),
		directory.NewMod(directory.NormalizeLower, nil),
		global.NewMod(nil),
		activity.NewMod(nil),
		mail.NewMod(nil, nil),
		match.NewMod(nil),
		platform.NewMod(
			platform.VerifierFunc(func(_ context.Context, c platform.Credential) (platform.Verified, error) {
				return platform.Verified{Channel: c.Channel, OpenID: c.OpenID}, nil
			}),
			platform.PlayerResolverFunc(func(context.Context, platform.Verified) (int64, error) { return 1001, nil }),
			platform.DelivererFunc(func(context.Context, platform.Order) error { return nil }),
			nil,
		),
		rank.NewMod(nil),
		session.NewMod(
			session.ReleaserFunc(func(context.Context, session.Run, session.Resource) error { return nil }),
			nil,
		),
	}
}

// bootstrap takes every Mod through Init and Provide against a registry
// holding a live Redis, the way an app does.
func bootstrap(t *testing.T) (*app.Registry, *viper.Viper) {
	t.Helper()
	c := client(t)
	cfg := modConfig(prefix(t, "mods"))
	registry := app.NewRegistry(cfg)
	// Stand in for roost-kit's RedisMod, which is what publishes this
	// capability in a real process.
	if err := registry.Register(mods.ModRedis, c); err != nil {
		t.Fatal(err)
	}
	for _, mod := range serviceMods(t) {
		if err := mod.Init(cfg); err != nil {
			t.Fatalf("%s Init: %v", mod.Name(), err)
		}
		if err := mod.Provide(registry); err != nil {
			t.Fatalf("%s Provide: %v", mod.Name(), err)
		}
		if err := mod.Start(); err != nil {
			t.Fatalf("%s Start: %v", mod.Name(), err)
		}
		t.Cleanup(mod.Stop)
	}
	return registry, cfg
}

// Every capability in the name table is published by some Mod, and none is
// missing. A name in the table that nothing registers is a name a business
// repo will look up and not find.
func TestEveryDeclaredCapabilityIsPublished(t *testing.T) {
	registry, _ := bootstrap(t)
	for _, name := range mods.All {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("capability %q is in the name table but no Mod published it", name)
		}
	}
}

// Each capability resolves to the type a caller will assert on, and the value
// WORKS — not merely that something non-nil is present. A registry holding a
// service that cannot reach its store is a process that starts and then fails
// on the first request.
func TestEveryPublishedCapabilityIsUsable(t *testing.T) {
	registry, _ := bootstrap(t)
	ctx := context.Background()

	t.Run("account", func(t *testing.T) {
		service, ok := app.Lookup[account.Accounts](registry, mods.ModAccount)
		if !ok {
			t.Fatal("the account capability is not an account.Accounts")
		}
		// Login only. Creating a role needs a server row, and UpsertServer is
		// NOT on the cross-process interface — registering and closing game
		// servers is a control-plane write, so a process holding the account
		// capability cannot make it. That is the intended shape, and this
		// subtest is the first place it becomes visible.
		acct, err := service.Login(ctx, account.Identity{Channel: "store", OpenID: "u1", Credential: "good"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateRole(ctx, acct.ID, 1, "Alice"); err == nil {
			t.Fatal("CreateRole succeeded with no server row; the server check did not run")
		}
	})

	t.Run("chat", func(t *testing.T) {
		service, ok := app.Lookup[chat.Messaging](registry, mods.ModChat)
		if !ok {
			t.Fatal("the chat capability is not a chat.Messaging")
		}
		if _, err := service.Publish(ctx, chat.Sender{RoleID: 1, Name: "role-1"}, chat.PublishRequest{
			Channel: chat.Channel{Kind: chat.ChannelWorld, Target: 7},
			Type:    "text", Body: []byte("hello"), RequestID: "r1",
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		dir, ok := app.Lookup[directory.Directory](registry, mods.ModDirectory)
		if !ok {
			t.Fatal("the directory capability is not a directory.Directory")
		}
		if _, err := dir.Reserve(ctx, "Zed", "owner-1", 0); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("global", func(t *testing.T) {
		service, ok := app.Lookup[global.Routing](registry, mods.ModGlobal)
		if !ok {
			t.Fatal("the global capability is not a global.Routing")
		}
		if _, err := service.Bind(ctx, 7, "group-a", 100); err != nil {
			t.Fatal(err)
		}
		if _, err := service.AcquireLease(ctx, 7); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("global activity", func(t *testing.T) {
		coordinator, ok := app.Lookup[activity.Coordinator](registry, mods.ModGlobalActivity)
		if !ok {
			t.Fatal("the global activity capability is not an activity.Coordinator")
		}
		key := activity.Key{GroupID: "group-a", ActivityID: "act-1", Phase: "settle"}
		if _, err := coordinator.OpenActivity(ctx, key, []int32{7}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("mail", func(t *testing.T) {
		// Looked up as the INTERFACE, which is the lookup a consumer writes
		// once for both deployments. Asserting on *mail.Service here compiled
		// and passed while mail ran in this process, and would have failed on
		// the day mail moved into its own — so mail's capability is now a
		// wrapper that satisfies mail.Mail and nothing else, and this test was
		// the first thing it caught.
		service, ok := app.Lookup[mail.Mail](registry, mods.ModMail)
		if !ok {
			t.Fatal("the mail capability is not a mail.Mail")
		}
		if _, err := service.Send(ctx, mail.SendRequest{
			Audience: mail.AudienceDirect, Recipients: []int64{7}, Subject: "reward",
			ExpiresInSeconds: 3600, RequestID: "r1",
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("match", func(t *testing.T) {
		store, ok := app.Lookup[match.Matchmaker](registry, mods.ModMatch)
		if !ok {
			t.Fatal("the match capability is not a match.Matchmaker")
		}
		if _, err := store.Enqueue(ctx,
			match.Queue{Mode: "ranked", GroupSize: 2, Partition: "eu"},
			match.Subject{Kind: "player", ID: 1, Score: 100}, "r1"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("platform", func(t *testing.T) {
		service, ok := app.Lookup[platform.Platform](registry, mods.ModPlatform)
		if !ok {
			t.Fatal("the platform capability is not a platform.Platform")
		}
		issued, err := service.AuthSession(ctx, platform.Credential{
			Channel: "store", OpenID: "u1", Secret: "anything",
		})
		if err != nil {
			t.Fatal(err)
		}
		// ValidateSession is deliberately NOT on the cross-process interface —
		// it takes no context and does no I/O, so making it a round trip would
		// queue every session check in the fleet behind one process. A caller
		// verifies the token locally against the same secret, which is what
		// the check below does, and it is a stronger assertion anyway.
		//
		// It verifies against the secret CONFIGURATION named, checked
		// independently of the service.
		//
		// Round-tripping through the same service proves nothing about which
		// secret was wired: a Mod that passed the payment secret where the
		// session secret belongs would sign and verify consistently with
		// itself, and only fail against anything else that reads the same
		// config — a gateway, a sibling process. That is a deployment bug that
		// self-verification cannot see, so this asserts the actual value.
		if _, err := security.VerifySessionToken(
			issued.Token, "itest-platform-session", issued.PlayerID, time.Now(),
		); err != nil {
			t.Fatalf("the token does not verify against platform.session_secret from "+
				"configuration: %v; the Mod wired a different secret", err)
		}
	})

	t.Run("rank", func(t *testing.T) {
		store, ok := app.Lookup[rank.Rank](registry, mods.ModRank)
		if !ok {
			t.Fatal("the rank capability is not a rank.Rank")
		}
		board := rank.Board{ID: "arena", Scope: rank.ScopeServer, ScopeID: 1, Season: 1}
		if _, err := store.Submit(ctx, board, rank.Score{OwnerID: 1, Value: 10}, rank.UpdateSet, ""); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("session", func(t *testing.T) {
		service, ok := app.Lookup[session.Session](registry, mods.ModSession)
		if !ok {
			t.Fatal("the session capability is not a session.Session")
		}
		if _, err := service.Enter(ctx, 1, session.EnterRequest{Kind: "d7", RequestID: "r1"}); err != nil {
			t.Fatal(err)
		}
	})
}

// The Mods' key prefixes come from configuration, so the whole set must land
// under the configured root. A Mod that hardcoded a prefix, or read the wrong
// config key, would write outside it — and two deployments sharing one Redis
// would then share state, which is exactly what the required prefix prevents.
//
// It is measured as a before/after difference over the WHOLE keyspace rather
// than by scanning for a suspected pattern. Scanning for one pattern only
// finds the strays you already guessed at, and it also picks up leftovers
// from earlier runs — which made an earlier version of this test report a
// stray that a previous test had written, turning two mutation checks green
// while looking red.
func TestEveryModWritesUnderItsConfiguredPrefix(t *testing.T) {
	c := client(t)
	root := prefix(t, "confined")
	cfg := modConfig(root)
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModRedis, c); err != nil {
		t.Fatal(err)
	}
	for _, mod := range serviceMods(t) {
		if err := mod.Init(cfg); err != nil {
			t.Fatalf("%s Init: %v", mod.Name(), err)
		}
		if err := mod.Provide(registry); err != nil {
			t.Fatalf("%s Provide: %v", mod.Name(), err)
		}
	}

	before := keySet(t, c)
	driveThroughRegistry(t, registry, root)
	after := keySet(t, c)

	fresh, stray := 0, []string{}
	for key := range after {
		if before[key] {
			continue
		}
		fresh++
		if !strings.HasPrefix(key, root) {
			stray = append(stray, key)
		}
	}
	if fresh == 0 {
		t.Fatal("no new keys were written; this test would pass vacuously")
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		t.Fatalf("%d of %d new keys were written outside the configured prefix %q, e.g. %q; "+
			"a hardcoded prefix or a misread config key means two deployments sharing one "+
			"redis share state", len(stray), fresh, root, stray[0])
	}

	// And each service writes under its OWN sub-prefix, not merely somewhere
	// under the root.
	//
	// "Under the root" is not enough on its own: a Mod that read another
	// service's config key — session reading rank.key_prefix, say — still
	// lands under the root, so the stray check above passes while two
	// services quietly share a namespace. That mutation came back green until
	// this assertion existed.
	for _, service := range []string{
		"account", "activity", "chat", "directory", "global", "mail",
		"match", "platform", "rank", "session",
	} {
		want := root + ":" + service + ":"
		found := false
		for key := range after {
			if !before[key] && strings.HasPrefix(key, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("nothing was written under %q; the %s mod is reading another service's "+
				"config key, so the two share a namespace", want, service)
		}
	}
	t.Logf("%d new keys, all under %q, each service in its own namespace", fresh, root)
}

// keySet snapshots the whole keyspace, so what a test measures is the
// DIFFERENCE it caused rather than whatever matched a guess.
func keySet(t *testing.T, c fredis.IRedis) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, key := range scanKeys(t, c, "*") {
		out[key] = true
	}
	return out
}

// driveThroughRegistry writes at least once through every capability, looked
// up rather than held — so the write really goes through what the Mod
// published.
func driveThroughRegistry(t *testing.T, registry *app.Registry, root string) {
	t.Helper()
	ctx := context.Background()

	// Looked up as the INTERFACE, here and below. Every service's Mod now
	// publishes a wrapper that satisfies its cross-process interface and
	// nothing else, so a concrete-type lookup does not resolve — which is the
	// property that makes a consumer deployment-independent, and the reason
	// this function reads the way a business repo's code will.
	//
	// One consequence is visible immediately: account's server list cannot be
	// written through the capability, because UpsertServer is a control-plane
	// operation and is not on the interface. So the role that needs a server
	// row is created directly against the store, which is what an operator
	// tool or a migration would do.
	acct := app.MustLookup[account.Accounts](registry, mods.ModAccount)
	acctStores, err := account.NewRedisStores(client(t), root+":account", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := acctStores.Servers.Create(ctx, 1, account.GameServer{
		ID: 1, Name: "s1", Status: account.ServerOpen,
	}); err != nil {
		t.Fatal(err)
	} else if !created {
		t.Fatal("the server row already existed; this test's prefix is not unique per run")
	}
	account1, err := acct.Login(ctx, account.Identity{Channel: "store", OpenID: "u1", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acct.CreateRole(ctx, account1.ID, 1, "Alice"); err != nil {
		t.Fatal(err)
	}

	chatSvc := app.MustLookup[chat.Messaging](registry, mods.ModChat)
	if _, err := chatSvc.Publish(ctx, chat.Sender{RoleID: 1, Name: "role-1"}, chat.PublishRequest{
		Channel: chat.Channel{Kind: chat.ChannelWorld, Target: 7},
		Type:    "text", Body: []byte("hello"), RequestID: "r1",
	}); err != nil {
		t.Fatal(err)
	}

	dir := app.MustLookup[directory.Directory](registry, mods.ModDirectory)
	if _, err := dir.Reserve(ctx, "Zed", "owner-1", 0); err != nil {
		t.Fatal(err)
	}

	globalSvc := app.MustLookup[global.Routing](registry, mods.ModGlobal)
	if _, err := globalSvc.Bind(ctx, 7, "group-a", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := globalSvc.AcquireLease(ctx, 7); err != nil {
		t.Fatal(err)
	}

	mailSvc := app.MustLookup[mail.Mail](registry, mods.ModMail)
	if _, err := mailSvc.Send(ctx, mail.SendRequest{
		Audience: mail.AudienceDirect, Recipients: []int64{7}, Subject: "reward",
		ExpiresInSeconds: 3600, RequestID: "r1",
	}); err != nil {
		t.Fatal(err)
	}

	matchStore := app.MustLookup[match.Matchmaker](registry, mods.ModMatch)
	if _, err := matchStore.Enqueue(ctx,
		match.Queue{Mode: "ranked", GroupSize: 2, Partition: "eu"},
		match.Subject{Kind: "player", ID: 1, Score: 100}, "r1"); err != nil {
		t.Fatal(err)
	}

	platformSvc := app.MustLookup[platform.Platform](registry, mods.ModPlatform)
	raw := []byte(`{"order_id":"o1","player_id":1001,"channel":"store",` +
		`"product_id":"gems-100","amount_minor":499,"currency":"USD"}`)
	if _, err := platformSvc.HandleCallback(ctx, raw,
		platform.SignPayload(raw, "itest-platform-payment")); err != nil {
		t.Fatal(err)
	}

	rankStore := app.MustLookup[rank.Rank](registry, mods.ModRank)
	if _, err := rankStore.Submit(ctx,
		rank.Board{ID: "arena", Scope: rank.ScopeServer, ScopeID: 1, Season: 1},
		rank.Score{OwnerID: 1, Value: 10}, rank.UpdateSet, ""); err != nil {
		t.Fatal(err)
	}

	sessionSvc := app.MustLookup[session.Session](registry, mods.ModSession)
	if _, err := sessionSvc.Enter(ctx, 1, session.EnterRequest{Kind: "d7", RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}

	coordinator := app.MustLookup[activity.Coordinator](registry, mods.ModGlobalActivity)
	if _, err := coordinator.OpenActivity(ctx,
		activity.Key{GroupID: "group-a", ActivityID: "act-1", Phase: activity.PhaseSettle},
		[]int32{7}); err != nil {
		t.Fatal(err)
	}
}

// --- the split-deployment shape ---

// mail's owning Mod publishes its capability as the INTERFACE WRAPPER, not as
// the local concrete type.
//
// This is the property that makes a consumer deployment-independent, and it
// has to be asserted against the real Mod because the wrapping happens in
// Provide. The subtlety it guards: converting at the call site —
// `Value: mail.Mail(service)` — does NOT achieve it, because Go stores an any
// whose dynamic type is still *Service and the assertion succeeds anyway.
func TestTheOwningModPublishesMailAsTheInterfaceOnly(t *testing.T) {
	c := client(t)
	cfg := modConfig(prefix(t, "shape"))
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModRedis, c); err != nil {
		t.Fatal(err)
	}
	mod := mail.NewMod(nil, nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if _, ok := app.Lookup[mail.Mail](registry, mods.ModMail); !ok {
		t.Fatal("the capability does not resolve as mail.Mail")
	}
	if _, concrete := app.Lookup[*mail.Service](registry, mods.ModMail); concrete {
		t.Fatal("the owning Mod published the concrete *mail.Service; a consumer can bind to " +
			"it, compile, pass, and then fail on the day mail moves into its own process")
	}
}

// EVERY owning Mod publishes its capability as the interface wrapper, and the
// concrete implementation type is not reachable through the registry.
//
// The mail-only version of this test above came first and is kept, because it
// exercises one Mod in isolation. This one is the generalization, and it is
// the test that matters: the four packages that got their transports before
// chat, platform, account, global and activity had integration subtests
// asserting on their CONCRETE types — app.Lookup[*session.Service] and
// friends — and those assertions passed for months because the suite skips
// without REDIS_ADDR. The day they ran, four of them failed at once.
//
// The property being asserted is what makes a consumer deployment-independent:
// a business repo writes app.Lookup[session.Session] once, and it resolves
// whether session runs in this process or answers over the bus. If the
// concrete type also resolved, a consumer could bind to it, compile, pass
// every test, and fail on the day that service moved out.
//
// The negative half is the load-bearing one, and it needs the wrapper to
// achieve: converting at the call site — Value: session.Session(service) —
// does NOT work, because Go stores an any whose dynamic type is still
// *Service and the concrete assertion succeeds anyway.
func TestEveryOwningModPublishesTheInterfaceAndNotTheImplementation(t *testing.T) {
	registry, _ := bootstrap(t)
	for _, probe := range []struct {
		pkg       string
		name      app.ModName
		asFace    func(*app.Registry, app.ModName) bool
		asConcret func(*app.Registry, app.ModName) bool
	}{
		{"account", mods.ModAccount,
			has[account.Accounts], has[*account.Service]},
		{"activity", mods.ModGlobalActivity,
			has[activity.Coordinator], has[*activity.Service]},
		{"chat", mods.ModChat,
			has[chat.Messaging], has[*chat.Service]},
		{"global", mods.ModGlobal,
			has[global.Routing], has[*global.Service]},
		{"mail", mods.ModMail,
			has[mail.Mail], has[*mail.Service]},
		{"match", mods.ModMatch,
			has[match.Matchmaker], has[match.Store]},
		{"platform", mods.ModPlatform,
			has[platform.Platform], has[*platform.Service]},
		{"rank", mods.ModRank,
			has[rank.Rank], has[rank.Store]},
		{"session", mods.ModSession,
			has[session.Session], has[*session.Service]},
	} {
		t.Run(probe.pkg, func(t *testing.T) {
			if !probe.asFace(registry, probe.name) {
				t.Fatalf("capability %q does not resolve as %s's cross-process interface; a "+
					"consumer written against the interface cannot use it", probe.name, probe.pkg)
			}
			if probe.asConcret(registry, probe.name) {
				t.Fatalf("capability %q resolves as %s's implementation type; a consumer can "+
					"bind to it, compile, pass, and then fail on the day %s moves into its own "+
					"process", probe.name, probe.pkg, probe.pkg)
			}
		})
	}
}

// has reports whether a capability resolves as T.
//
// It exists so the table above can hold one lookup per type: app.Lookup's type
// parameter cannot come from a struct field, so each row carries an
// instantiation — has[account.Accounts] — instead of a type name.
func has[T any](registry *app.Registry, name app.ModName) bool {
	_, ok := app.Lookup[T](registry, name)
	return ok
}

// The owning Mod and the client Mod cannot both live in one process.
//
// They publish the same capability name, so the registry refuses the second.
// That is the desired behaviour: a process holding both a local service and a
// client to itself would forward requests to itself, and which one a consumer
// got would depend on registration order.
func TestTheTwoMailModsCannotShareAProcess(t *testing.T) {
	c := client(t)
	cfg := modConfig(prefix(t, "exclusive"))
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModRedis, c); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModBus, stubBus{}); err != nil {
		t.Fatal(err)
	}

	owner := mail.NewMod(nil, nil)
	if err := owner.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := owner.Provide(registry); err != nil {
		t.Fatalf("the owning Mod failed to provide: %v", err)
	}

	remote := mail.NewClientMod()
	if err := remote.Init(cfg); err != nil {
		t.Fatal(err)
	}
	err := remote.Provide(registry)
	if err == nil {
		t.Fatal("both mail Mods provided into one registry")
	}
	if !strings.Contains(err.Error(), string(mods.ModMail)) {
		t.Fatalf("the error does not name the conflicting capability: %v", err)
	}

	// And the other order fails too, so the exclusion is not an artefact of
	// which one happens to be registered first.
	reversed := app.NewRegistry(cfg)
	if err := reversed.Register(mods.ModRedis, c); err != nil {
		t.Fatal(err)
	}
	if err := reversed.Register(mods.ModBus, stubBus{}); err != nil {
		t.Fatal(err)
	}
	remote2 := mail.NewClientMod()
	if err := remote2.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := remote2.Provide(reversed); err != nil {
		t.Fatal(err)
	}
	owner2 := mail.NewMod(nil, nil)
	if err := owner2.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := owner2.Provide(reversed); err == nil {
		t.Fatal("the owning Mod provided over an existing client capability")
	}
}

// The Server registers the RPC handlers; the Mod does not.
//
// Registering them in the Mod would publish mail's handlers in any process
// that happened to load it. Only a process running mail.Server is the owner,
// and being the owner is what registering handlers means.
func TestOnlyTheMailServerRegistersHandlers(t *testing.T) {
	c := client(t)
	cfg := modConfig(prefix(t, "handlers"))
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModRedis, c); err != nil {
		t.Fatal(err)
	}
	counting := &countingBus{}
	if err := registry.Register(mods.ModBus, counting); err != nil {
		t.Fatal(err)
	}

	mod := mail.NewMod(nil, nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if got := counting.registered(); got != 0 {
		t.Fatalf("the Mod registered %d handlers; only the Server is the owner", got)
	}

	if err := mail.NewServer().Init(registry); err != nil {
		t.Fatal(err)
	}
	if got := counting.registered(); got != len(mail.Methods) {
		t.Fatalf("the Server registered %d handlers, want %d", got, len(mail.Methods))
	}
}

// The CLIENT Mod must not register handlers either, and that is the more
// consequential half.
//
// A process that only calls mail would otherwise publish mail's handlers and
// start answering for it — with a client as its implementation, so every
// request it answered would be forwarded onward. The owning Mod registering
// them is a duplicate; the client Mod registering them is a second, wrong
// owner.
func TestTheMailClientModRegistersNoHandlers(t *testing.T) {
	cfg := modConfig(prefix(t, "clienthandlers"))
	registry := app.NewRegistry(cfg)
	counting := &countingBus{}
	if err := registry.Register(mods.ModBus, counting); err != nil {
		t.Fatal(err)
	}
	mod := mail.NewClientMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if got := counting.registered(); got != 0 {
		t.Fatalf("the client Mod registered %d handlers; a process that only calls mail would "+
			"answer for it, forwarding every request onward", got)
	}
	// And a Server cannot be started here: the owner-only capability is absent,
	// which is how a process knows it does not hold the implementation. The
	// error names it, so the fix is legible.
	err := mail.NewServer().Init(registry)
	if err == nil {
		t.Fatal("a Server started in a process holding only a client capability; it would " +
			"forward every request to itself")
	}
	if !strings.Contains(err.Error(), string(mail.LocalCapabilityName)) {
		t.Fatalf("the error does not name the owner-only capability: %v", err)
	}
}

// --- bus doubles ---

// stubBus satisfies the bus capability for tests that only need it to exist.
// Every method fails loudly: a test that started actually calling the bus
// should have to say so rather than silently succeeding against a no-op.
type stubBus struct{}

func (stubBus) Send(int32, string, any) error { return errors.New("stubBus: Send") }
func (stubBus) SendByType(string, int32, string, any) error {
	return errors.New("stubBus: SendByType")
}
func (stubBus) Broadcast(string, string, any) error { return errors.New("stubBus: Broadcast") }
func (stubBus) BroadcastAll(string, any) error      { return errors.New("stubBus: BroadcastAll") }
func (stubBus) Call(context.Context, string, string, any, any) error {
	return errors.New("stubBus: Call")
}
func (stubBus) CallTo(context.Context, string, int32, string, any, any) error {
	return errors.New("stubBus: CallTo")
}
func (stubBus) CallWithTimeout(string, string, any, any, time.Duration) error {
	return errors.New("stubBus: CallWithTimeout")
}
func (stubBus) CallAsync(string, string, any, func([]byte, error)) {}
func (stubBus) Handle(string, string, bus.HandlerFunc) error {
	return errors.New("stubBus: Handle")
}
func (stubBus) HandleRpc(string, bus.RpcHandlerFunc) error { return nil }

var _ bus.IBus = stubBus{}

// countingBus counts handler registrations, which is the only thing the
// ownership test needs to observe.
type countingBus struct {
	stubBus
	mu    sync.Mutex
	names map[string]bool
}

func (c *countingBus) HandleRpc(method string, _ bus.RpcHandlerFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names == nil {
		c.names = map[string]bool{}
	}
	if c.names[method] {
		return fmt.Errorf("countingBus: %s already registered", method)
	}
	c.names[method] = true
	return nil
}

func (c *countingBus) registered() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.names)
}

var _ bus.IBus = (*countingBus)(nil)

// The central name table and each service's generated CapabilityName must
// agree.
//
// They are two literals of the same name — mods.All exists so a
// collision across services is visible by reading one file, and the generated
// constant exists so a service's transport does not depend on that file. Two
// literals drift, so the equality is asserted rather than trusted: a Mod
// ordered under one name and looked up under another is a capability nobody
// can resolve, and the failure would be a startup error naming a name that
// appears correct in both places.
func TestTheNameTableAgreesWithEachGeneratedCapability(t *testing.T) {
	// The pairing is written out per package rather than derived. Deriving it
	// — matching a generated name against the table by string — would pass
	// whenever SOME table entry happened to equal the generated name, which
	// is exactly what a rename produces: mail's capability silently satisfied
	// by match's entry. Each row names both sides, so a rename in either
	// place fails here.
	pairs := []struct {
		pkg       string
		generated app.ModName
		table     app.ModName
	}{
		{"account", account.CapabilityName, mods.ModAccount},
		{"global", global.CapabilityName, mods.ModGlobal},
		{"chat", chat.CapabilityName, mods.ModChat},
		{"activity", activity.CapabilityName, mods.ModGlobalActivity},
		{"mail", mail.CapabilityName, mods.ModMail},
		{"match", match.CapabilityName, mods.ModMatch},
		{"platform", platform.CapabilityName, mods.ModPlatform},
		{"rank", rank.CapabilityName, mods.ModRank},
		{"session", session.CapabilityName, mods.ModSession},
	}
	table := map[app.ModName]bool{}
	for _, name := range mods.All {
		table[name] = true
	}
	seen := map[app.ModName]string{}
	for _, pair := range pairs {
		if pair.generated != pair.table {
			t.Fatalf("%s generates capability %q but the name table says %q",
				pair.pkg, pair.generated, pair.table)
		}
		if !table[pair.generated] {
			t.Fatalf("%s generates capability %q, which mods.All does not list; a "+
				"collision with another service would be invisible", pair.pkg, pair.generated)
		}
		if other, dup := seen[pair.generated]; dup {
			t.Fatalf("%s and %s both publish capability %q; one would overwrite the other in "+
				"a registry", pair.pkg, other, pair.generated)
		}
		seen[pair.generated] = pair.pkg
	}
}

// The owner-only capability is the public one plus ".local", for every
// generated package.
//
// The Server decides whether this process holds the implementation by looking
// up the local name, so a package where the two names drift apart is a package
// whose Server never starts — and the failure is silent: Init finds no local
// capability and concludes it holds a client. Deriving the name in the
// template is what keeps them together; this asserts the derivation.
func TestEveryOwnerOnlyCapabilityIsThePublicNamePlusLocal(t *testing.T) {
	pairs := []struct {
		pkg    string
		public app.ModName
		local  app.ModName
	}{
		{"account", account.CapabilityName, account.LocalCapabilityName},
		{"global", global.CapabilityName, global.LocalCapabilityName},
		{"chat", chat.CapabilityName, chat.LocalCapabilityName},
		{"activity", activity.CapabilityName, activity.LocalCapabilityName},
		{"mail", mail.CapabilityName, mail.LocalCapabilityName},
		{"match", match.CapabilityName, match.LocalCapabilityName},
		{"platform", platform.CapabilityName, platform.LocalCapabilityName},
		{"rank", rank.CapabilityName, rank.LocalCapabilityName},
		{"session", session.CapabilityName, session.LocalCapabilityName},
	}
	seen := map[app.ModName]string{}
	for _, pair := range pairs {
		if want := pair.public + ".local"; pair.local != want {
			t.Fatalf("%s: owner-only capability is %q, want %q", pair.pkg, pair.local, want)
		}
		// And it must not collide with any public name, or a process holding
		// one service's client would look like the owner of another.
		for _, other := range pairs {
			if pair.local == other.public {
				t.Fatalf("%s's owner-only name %q is %s's public name", pair.pkg, pair.local, other.pkg)
			}
		}
		if other, dup := seen[pair.local]; dup {
			t.Fatalf("%s and %s share the owner-only name %q", pair.pkg, other, pair.local)
		}
		seen[pair.local] = pair.pkg
	}
}

// The operator surface is reachable in the owning process and NOWHERE ELSE.
//
// Each Admin method is more dangerous than anything on its service's
// cross-process interface — one can cause a second grant of paid goods, one
// re-delivers an activity result, one declares an external resource gone —
// and the bus carries no caller identity these services can verify. So none of
// them has a //roost:rpc marker, and the capability other processes hold must
// not satisfy Admin.
//
// Asserted from the registry rather than in each package's own tests, because
// the property being checked is about what the MOD PUBLISHED: a package test
// can only look at a value it constructed itself.
func TestTheOperatorSurfacesAreNotReachableThroughTheBusCapability(t *testing.T) {
	registry, _ := bootstrap(t)
	for _, probe := range []struct {
		pkg string
		// public is the name every consumer looks up; asAdmin tries to reach
		// the operator surface through it.
		public  app.ModName
		asAdmin func(*app.Registry, app.ModName) bool
		// local is the owner-only name, where the implementation lives and
		// where Admin IS expected to be reachable.
		local app.ModName
	}{
		{"platform", mods.ModPlatform, has[platform.Admin], platform.LocalCapabilityName},
		{"session", mods.ModSession, has[session.Admin], session.LocalCapabilityName},
		{"activity", mods.ModGlobalActivity, has[activity.Admin], activity.LocalCapabilityName},
	} {
		t.Run(probe.pkg, func(t *testing.T) {
			if probe.asAdmin(registry, probe.public) {
				t.Fatalf("%s's PUBLIC capability %q satisfies Admin; any process on the bus "+
					"could reach the operator surface, and the bus carries no identity this "+
					"service can verify", probe.pkg, probe.public)
			}
			// And the owning process CAN reach it — otherwise the surface
			// exists but nothing can call it, which is the dead end again.
			if !probe.asAdmin(registry, probe.local) {
				t.Fatalf("%s's owner-only capability %q does not satisfy Admin; the operator "+
					"surface exists but the owning process cannot reach it", probe.pkg, probe.local)
			}
		})
	}
}

// The owner-only capability holds the IMPLEMENTATION, not the wrapper.
//
// This pins the fix directly rather than through a run hook, which matters
// because it holds for the packages whose hooks do not need the concrete type
// today — account, global, mail, rank. A future hook there that reaches an
// owner-only method must not be the thing that discovers this.
//
// The two halves together are the whole contract: the PUBLIC name must not
// resolve as the implementation (a consumer would bind to it and break on the
// day the service moves out), and the OWNER-ONLY name must. Asserting only one
// side leaves the other free to drift, and the first version of this design
// had the wrapper under both names.
func TestTheOwnerOnlyCapabilityHoldsTheImplementation(t *testing.T) {
	registry, _ := bootstrap(t)
	for _, probe := range []struct {
		pkg   string
		local app.ModName
		// asImpl looks the owner-only name up as the implementation type.
		asImpl func(*app.Registry, app.ModName) bool
	}{
		{"account", account.LocalCapabilityName, has[*account.Service]},
		{"activity", activity.LocalCapabilityName, has[*activity.Service]},
		{"chat", chat.LocalCapabilityName, has[*chat.Service]},
		{"global", global.LocalCapabilityName, has[*global.Service]},
		{"mail", mail.LocalCapabilityName, has[*mail.Service]},
		{"match", match.LocalCapabilityName, has[match.Store]},
		{"platform", platform.LocalCapabilityName, has[*platform.Service]},
		{"rank", rank.LocalCapabilityName, has[rank.Store]},
		{"session", session.LocalCapabilityName, has[*session.Service]},
	} {
		t.Run(probe.pkg, func(t *testing.T) {
			if !probe.asImpl(registry, probe.local) {
				t.Fatalf("owner-only capability %q does not resolve as %s's implementation "+
					"type; the run hook cannot reach the owner-only methods that are "+
					"deliberately kept off the cross-process interface — sweeps, retries, "+
					"pruning — so Serve fails and the owning process cannot start",
					probe.local, probe.pkg)
			}
		})
	}
}

// EVERY owning process can actually Serve.
//
// This is the test that was missing, and its absence hid a process-fatal bug in
// five packages.
//
// The owning Mod publishes a capability WRAPPER (see the test above: that is
// what stops a consumer binding to the implementation type). Server.Init takes
// that value out of the registry and Service() hands it to the hand-written
// run hook — so a hook that asserts the concrete type, which it must in order
// to reach the owner-only methods deliberately kept off the cross-process
// interface, was asserting against the wrapper and failing. match, platform,
// chat and global/activity returned an error from run, meaning Serve failed
// and the process could not start. session's was a BARE assertion inside the
// sweep ticker, so it panicked thirty seconds in — and only in a deployment
// that had supplied owners to sweep, which is worse rather than better.
//
// Nothing caught it because nothing ever called Serve. TestOnlyTheMailServer…
// covers Init for one package, and Init is exactly the half that worked.
//
// The context is cancelled almost immediately: what is under test is that the
// hook can resolve its implementation at all, which every hook now does before
// entering its loop. A hook that resolves lazily inside a ticker would pass
// this and still fail in production, so "resolve up front" is part of the
// contract, not an accident of how this test is written.
func TestEveryServerServesInTheOwningProcess(t *testing.T) {
	registry, cfg := bootstrap(t)
	if err := registry.Register(mods.ModBus, &countingBus{}); err != nil {
		t.Fatal(err)
	}
	_ = cfg
	for _, probe := range []struct {
		pkg    string
		server app.Service
	}{
		{"account", account.NewServer()},
		{"activity", activity.NewServer()},
		{"chat", chat.NewServer()},
		{"global", global.NewServer()},
		{"mail", mail.NewServer()},
		{"match", match.NewServer()},
		{"platform", platform.NewServer()},
		{"rank", rank.NewServer()},
		{"session", session.NewServer()},
	} {
		t.Run(probe.pkg, func(t *testing.T) {
			if err := probe.server.Init(registry); err != nil {
				t.Fatalf("%s Server.Init: %v", probe.pkg, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := probe.server.Serve(ctx); err != nil {
				t.Fatalf("%s Server.Serve returned %v; the owning process cannot start", probe.pkg, err)
			}
			if err := probe.server.Shutdown(context.Background()); err != nil {
				t.Fatalf("%s Server.Shutdown: %v", probe.pkg, err)
			}
		})
	}
}
