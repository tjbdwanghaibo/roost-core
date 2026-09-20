package roost

import (
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

// frameworkServiceSpec describes one roost-service service a project can host
// as its own process (services.<name>.framework) and call from a business
// service (services.<name>.uses).
//
// Hosting means the generated bootstrap registers the service's Server as a
// subcommand with its owner Mod, on the Kit mods it depends on. Calling means
// the business service's process gets the ClientMod and a typed accessor.
// The collaborators the owner Mod requires — a Deliverer, a policy, an
// identity verifier — are business decisions, so they live in a file the
// generator creates once and never overwrites, with fail-closed defaults.
type frameworkServiceSpec struct {
	Package string
	// Path is where the package lives under roost-kit/service/, when that is
	// not the package name — activity is service/global/activity. Package
	// stays the Go package name because it is also the identifier the
	// generated bootstrap imports it as, and `svcglobal/activity` is not one.
	Path      string
	Interface string
	Depends   []string
	ModArgs   []string
	// ModChain is the optional collaborators a hosted service wires AFTER
	// NewMod, as chained calls on the returned Mod (kit's own shape for a
	// collaborator that has a defensible "none" — platform's pending-order
	// index is the first). Each entry is a method call whose argument names a
	// function in the project's collaborators file, so the default file must
	// define it too.
	ModChain   []string
	Collabs    string
	ConfigFunc func(project string) string
}

// ImportPath is the package's path under roost-kit/service/.
func (spec frameworkServiceSpec) ImportPath() string {
	if strings.TrimSpace(spec.Path) != "" {
		return spec.Path
	}
	return spec.Package
}

// frameworkServiceModule is the import root of the hosted services; since the
// consolidation they live in roost-kit/service/<name>.
const frameworkServiceModule = "github.com/tjbdwanghaibo/roost-kit/service"

var frameworkCatalog = map[string]frameworkServiceSpec{
	"account": {
		Package: "account", Interface: "Accounts", Depends: []string{"redis", "nats"},
		ModArgs: []string{"Verifier()", "Allocator()", "NameRules()", "Metrics()"},
		Collabs: `// Verifier confirms a login identity with its channel (platform SDK,
// device attestation, …). The default refuses every login: an account
// service that cannot verify identities must not issue sessions.
func Verifier() account.IdentityVerifier {
	return account.VerifierFunc(func(context.Context, account.Identity) (account.Verified, error) {
		return account.Verified{}, errors.New("account: identity verifier is not configured; implement Verifier() in internal/service/%[1]s/collaborators.go")
	})
}

// Allocator mints player ids per game server. The default refuses: ids must be
// globally unique and come from a source this project chooses.
func Allocator() account.PlayerIDAllocator {
	return account.AllocatorFunc(func(context.Context, int32) (int64, error) {
		return 0, errors.New("account: player id allocator is not configured; implement Allocator() in internal/service/%[1]s/collaborators.go")
	})
}

// NameRules validates role names. The default bounds length and refuses
// whitespace; replace it with the project's own rules.
func NameRules() account.NameValidator {
	return account.NameValidatorFunc(func(raw string) error {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || len(trimmed) > 32 {
			return errors.New("name must be 1–32 characters")
		}
		if strings.ContainsAny(trimmed, " \t\n") {
			return errors.New("name must not contain whitespace")
		}
		return nil
	})
}
`,
		ConfigFunc: func(project string) string {
			return "account:\n  key_prefix: roost:" + project + ":account\n  session_secret: CHANGE_ME\n  session_ttl: 30m\n  claim_ttl: 5m\n"
		},
	},
	"mail": {
		Package: "mail", Interface: "Mail", Depends: []string{"redis", "nats"},
		ModArgs: []string{"Broadcast()", "Metrics()"},
		Collabs: `// Broadcast delivers broadcast mail to every online player. nil is a valid,
// fail-closed configuration: broadcasts are refused rather than dropped.
func Broadcast() mail.Deliverer { return nil }
`,
		ConfigFunc: func(project string) string {
			return "mail:\n  key_prefix: roost:" + project + ":mail\n  send_ttl: 720h\n  claim_lease: 30s\n"
		},
	},
	"match": {
		Package: "match", Interface: "Matchmaker", Depends: []string{"redis", "nats"},
		ModArgs: []string{"Metrics()"},
		Collabs: `// The match service takes no matchmaking policy: it holds the queue and makes
// Commit atomic, and deciding which waiting tickets form a match is the game's
// job — a matchmaker in the game process reads Candidates, applies a
// match.Grouping (FirstComeGrouping, ScoreWindowGrouping or its own) and
// Commits. See the game-demo template's internal/service/<game>/matchmaker.go.
`,
		ConfigFunc: func(project string) string {
			return "match:\n  key_prefix: roost:" + project + ":match\n  ticket_ttl: 60s\n  sweep_queues: []\n"
		},
	},
	"platform": {
		Package: "platform", Interface: "Platform", Depends: []string{"redis", "nats"},
		ModArgs:  []string{"Verify()", "Players()", "Deliver()", "Metrics()"},
		ModChain: []string{"WithPendingOrders(Pending())"},
		Collabs: `// Verify checks a provider callback's signature. The default refuses: a
// platform service that cannot tell a real callback from a forged one must not
// deliver anything, and "accept everything in development" is the setting that
// reaches production.
func Verify() platform.Verifier {
	return platform.VerifierFunc(func(context.Context, platform.Credential) (platform.Verified, error) {
		return platform.Verified{}, errors.New("platform: callback verifier is not configured; implement Verify() in internal/service/%[1]s/collaborators.go")
	})
}

// Players resolves a channel account to this server's player id.
func Players() platform.PlayerResolver {
	return platform.PlayerResolverFunc(func(context.Context, platform.Verified) (int64, error) {
		return 0, errors.New("platform: player resolver is not configured; implement Players() in internal/service/%[1]s/collaborators.go")
	})
}

// Deliver grants what was paid for. It is called at most once per order by
// the service, but it MUST be idempotent anyway: the service retries after a
// failure it could not classify, and a grant that ran and then failed to
// report looks identical to one that never ran.
func Deliver() platform.Deliverer {
	return platform.DelivererFunc(func(context.Context, platform.Order) error {
		return errors.New("platform: deliverer is not configured; implement Deliver() in internal/service/%[1]s/collaborators.go")
	})
}

// Pending is this deployment's index of paid-but-undelivered orders: what the
// background retry loop reads every tick. nil is a defensible answer — a
// channel that re-delivers its callbacks recovers without one — and it is the
// default here because an index is a durable structure only the deployment can
// place. The Server says at start which mode it is in, so "off" is a visible
// choice rather than a silent one.
//
// An implementation keeps entries while an order is not terminal, retires them
// when it is, pages within the limit it is given, and survives a restart.
// It may implement platform.RegistryBound to receive the process's registry.
func Pending() platform.PendingOrders { return nil }
`,
		ConfigFunc: func(project string) string {
			// session_secret and payment_secret are refused when empty at Init
			// (an unset payment secret turns every provider callback into an
			// invalid-signature refusal), so they are emitted as CHANGE_ME
			// rather than omitted: a starter config whose process cannot start
			// reads as a broken generator.
			return "platform:\n  key_prefix: roost:" + project + ":platform\n  session_secret: CHANGE_ME\n  payment_secret: CHANGE_ME\n  session_ttl: 30m\n  delivery_attempts: 8\n"
		},
	},
	"global": {
		Package: "global", Interface: "Routing", Depends: []string{"redis", "nats"},
		ModArgs: []string{"Metrics()"},
		Collabs: `// The global service takes no collaborators: a route is a binding between a
// game server and a coordination group, and a lease is that server saying it
// is still alive. Both are state this service owns outright, so there is no
// policy for a project to supply — what a project decides is who calls Bind
// and who drives a migration, and those are callers, not collaborators.
`,
		ConfigFunc: func(project string) string {
			return "global:\n  key_prefix: roost:" + project + ":global\n  lease_ttl: 30s\n"
		},
	},
	"activity": {
		Package: "activity", Path: "global/activity", Interface: "Coordinator",
		Depends: []string{"redis", "nats"},
		ModArgs: []string{"Metrics()"},
		Collabs: `// The activity service takes no collaborators: it aggregates what game
// servers report and says when a phase is collected. What the phase MEANS —
// which activity, what a point is worth, what the settlement pays — is the
// game's, and it stays in the game process, on the two sides of this service:
// the progress it applies and the dispatch it acks.
`,
		ConfigFunc: func(project string) string {
			// reservation_ttl is required with no default: it must exceed the
			// caller's longest retry horizon, past which a replayed progress
			// request is indistinguishable from a new one and is applied
			// twice. Only the caller's transport knows that number, so the
			// service refuses to pick one — and a starter config that omits
			// it is a process that cannot start.
			return "activity:\n  key_prefix: roost:" + project + ":activity\n  reservation_ttl: 30m\n  grace_window: 60s\n  dispatch_attempts: 5\n  dispatch_backoff: 5s\n  sweep_groups: []\n"
		},
	},
	"rank": {
		Package: "rank", Interface: "Rank", Depends: []string{"redis", "nats"},
		ModArgs: []string{"Metrics()"},
		Collabs: `// The rank service takes no collaborators: what is ranked, how a submit
// combines with the stored value and when a season ends are all the caller's
// (a board id, an UpdateMode and a RequestID per submit). Reset is
// deliberately absent from the bus interface — emptying a board is an
// operator action with an audit trail, not something every peer can reach.
`,
		ConfigFunc: func(project string) string {
			return "rank:\n  key_prefix: roost:" + project + ":rank\n"
		},
	},
	"session": {
		Package: "session", Interface: "Session", Depends: []string{"redis", "nats"},
		ModArgs: []string{"Release()", "Metrics()"},
		Collabs: `// Release frees a resource a run attached — a dungeon instance, a seat, a
// reservation on another service — when the run finishes, is left or expires.
// The session service calls it exactly once per attached resource; the default
// refuses, so a run cannot look released while the instance is still held.
func Release() session.Releaser {
	return session.ReleaserFunc(func(context.Context, session.Run, session.Resource) error {
		return errors.New("session: releaser is not configured; implement Release() in internal/service/%[1]s/collaborators.go")
	})
}
`,
		ConfigFunc: func(project string) string {
			return "session:\n  key_prefix: roost:" + project + ":session\n  run_ttl: 30m\n  request_ttl: 1h\n"
		},
	},
	"chat": {
		Package: "chat", Interface: "Messaging", Depends: []string{"redis", "nats"},
		ModArgs: []string{"Policy()", "Bodies()", "System()", "Rules()", "Metrics()"},
		Collabs: `// Policy decides who may publish to and read from a channel. The default
// refuses everything: a permissive policy is what a client-settable trust
// flag amounted to in the implementation roost-service replaces.
func Policy() chat.ChannelPolicy {
	return chat.PolicyFuncs{
		Publish: func(context.Context, chat.Sender, chat.Channel) error {
			return errors.New("chat: publish policy is not configured; implement Policy() in internal/service/%[1]s/collaborators.go")
		},
		Read: func(context.Context, chat.Sender, chat.Channel) error {
			return errors.New("chat: read policy is not configured; implement Policy() in internal/service/%[1]s/collaborators.go")
		},
	}
}

// Bodies registers the message body types this game allows.
func Bodies() *chat.BodyRegistry { return chat.NewBodyRegistry() }

// System authenticates the privileged system entry point from transport
// identity. The default refuses, so there is no system path until one is
// deliberately granted.
func System() chat.SystemAuthenticator {
	return chat.SystemAuthenticatorFunc(func(context.Context) (chat.SystemToken, error) {
		return chat.SystemToken{}, errors.New("chat: system authenticator is not configured")
	})
}

// Rules are the channel kinds and their scoping.
func Rules() []chat.ChannelRule { return chat.DefaultChannelRules() }
`,
		ConfigFunc: func(project string) string {
			return "chat:\n  key_prefix: roost:" + project + ":chat\n  retention_age: 168h\n  prune_channels: []\n"
		},
	},
}

func frameworkServiceNames() []string {
	names := make([]string, 0, len(frameworkCatalog))
	for name := range frameworkCatalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// isFrameworkService reports whether the named service hosts a roost-service
// service instead of business code.
func (m Manifest) isFrameworkService(name string) bool {
	return strings.TrimSpace(m.Services[name].Framework) != ""
}

// usesFrameworkServices reports whether anything in the project depends on
// roost-service: a hosted service or a business service that calls one.
func (m Manifest) usesFrameworkServices() bool {
	for _, service := range m.Services {
		if strings.TrimSpace(service.Framework) != "" || len(service.Uses) > 0 {
			return true
		}
	}
	return false
}

// effectiveServiceMods is a service's Kit mod set as the generator assembles
// it: the declared mods, plus what a hosted framework service depends on
// (Redis for state, NATS for the bus), plus NATS for a business service that
// calls a framework service through its ClientMod or owns / calls a project
// rpc.
func effectiveServiceMods(m Manifest, name string) []string {
	service := m.Services[name]
	mods := append([]string(nil), service.Mods...)
	if spec, ok := frameworkCatalog[strings.TrimSpace(service.Framework)]; ok {
		mods = append(mods, spec.Depends...)
	}
	if len(service.Uses) > 0 || len(service.Rpcs) > 0 || len(service.UsesRpcs) > 0 {
		// The bus: a framework ClientMod, a project rpc's owner Mod (it
		// registers handlers) and a project rpc's ClientMod all ride on it.
		mods = append(mods, "nats")
	}
	return mods
}

func (m Manifest) validateFrameworkServices() error {
	var joined error
	names := sortedServiceNames(m)
	for _, name := range names {
		service := m.Services[name]
		framework := strings.TrimSpace(service.Framework)
		if framework != "" {
			if _, ok := frameworkCatalog[framework]; !ok {
				joined = errors.Join(joined, fmt.Errorf("service %s: unknown framework service %q; known: %s", name, framework, strings.Join(frameworkServiceNames(), ", ")))
			}
			if len(service.Uses) > 0 {
				joined = errors.Join(joined, fmt.Errorf("service %s hosts %s and cannot also declare uses; call other services from a business service", name, framework))
			}
			if access, ok := m.Access["player"]; ok && access.Service == name {
				joined = errors.Join(joined, fmt.Errorf("access.player cannot target framework service %s", name))
			}
		}
		seen := map[string]bool{}
		for _, used := range service.Uses {
			if seen[used] {
				joined = errors.Join(joined, fmt.Errorf("service %s repeats uses %q", name, used))
			}
			seen[used] = true
			target, ok := m.Services[used]
			if !ok || strings.TrimSpace(target.Framework) == "" {
				joined = errors.Join(joined, fmt.Errorf("service %s uses %q, which is not a framework service declared in services", name, used))
			}
		}
	}
	return joined
}

// renderFrameworkCollaborators is the business-owned file a hosted framework
// service's owner Mod is built from. Created once, never overwritten.
func renderFrameworkCollaborators(m Manifest, name string) string {
	spec := frameworkCatalog[m.Services[name].Framework]
	body := strings.ReplaceAll(spec.Collabs, "%[1]s", name)
	needsErrors := strings.Contains(body, "errors.New")
	needsStrings := strings.Contains(body, "strings.")
	needsContext := strings.Contains(body, "context.Context")
	var imports []string
	if needsContext {
		imports = append(imports, `"context"`)
	}
	if needsErrors {
		imports = append(imports, `"errors"`)
	}
	if needsStrings {
		imports = append(imports, `"strings"`)
	}
	imports = append(imports, "", `"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"`)
	// The service package is imported only when the collaborators body
	// references it. A body that supplies nothing but Metrics() — match, since
	// U-0217 removed its Grouping() — would otherwise ship an unused import,
	// and the generated project would not compile (U-0218). Comments that
	// mention `match.Grouping` do not count: the check is on the AST.
	if bodyUsesPackage(body, spec.Package) {
		imports = append(imports, fmt.Sprintf("%q", frameworkServiceModule+"/"+spec.ImportPath()))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "// Package %s supplies the collaborators the %s service needs from this\n// project. roost-codegen created this file once and will not overwrite it.\n", safeIdent(name), spec.Package)
	fmt.Fprintf(&b, "package %s\n\nimport (\n", safeIdent(name))
	for _, imp := range imports {
		if imp == "" {
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(&b, "\t%s\n", imp)
	}
	b.WriteString(")\n\n")
	b.WriteString(body)
	b.WriteString("\n// Metrics receives the service's counters. nil means no reporting and never\n// fails an operation; wire the project's servicemetrics.Reporter here.\nfunc Metrics() servicemetrics.Reporter { return nil }\n")
	return b.String()
}

// bodyUsesPackage reports whether the Go declarations in body refer to
// pkg through a selector expression (pkg.Something). It parses rather than
// searches text so that a mention inside a comment does not count.
func bodyUsesPackage(body, pkg string) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "collaborators.go", "package p\n"+body, 0)
	if err != nil {
		// An unparsable body is caught by the caller's format.Source; be
		// conservative and keep the import so the error is the real one.
		return true
	}
	used := false
	ast.Inspect(file, func(n ast.Node) bool {
		if used {
			return false
		}
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == pkg {
				used = true
			}
		}
		return true
	})
	return used
}

// renderFrameworkClients is the generated accessor file for a business
// service that calls framework services: one typed lookup per `uses` entry.
func renderFrameworkClients(m Manifest, name string) string {
	service := m.Services[name]
	used := append([]string(nil), service.Uses...)
	sort.Strings(used)
	var b strings.Builder
	b.WriteString(generatedHeader + "\n")
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"fmt\"\n\n\t\"github.com/tjbdwanghaibo/roost-core/app\"\n", safeIdent(name))
	for _, target := range used {
		spec := frameworkCatalog[m.Services[target].Framework]
		fmt.Fprintf(&b, "\tsvc%s %q\n", spec.Package, frameworkServiceModule+"/"+spec.ImportPath())
	}
	b.WriteString(")\n")
	for _, target := range used {
		spec := frameworkCatalog[m.Services[target].Framework]
		fmt.Fprintf(&b, `
// %[1]s returns the %[2]s service this process reaches through %[2]s.NewClientMod
// (roost.yaml: services.%[3]s.uses). Call it from Init and keep the result.
func %[1]s(r *app.Registry) (svc%[2]s.%[4]s, error) {
	service, ok := app.Lookup[svc%[2]s.%[4]s](r, svc%[2]s.CapabilityName)
	if !ok || service == nil {
		return nil, fmt.Errorf("%[3]s: capability %%q not found; is the %[2]s process running and reachable over the bus?", svc%[2]s.CapabilityName)
	}
	return service, nil
}
`, safeIdent(target), spec.Package, name, spec.Interface)
	}
	return b.String()
}

// applyGameTemplate turns a fresh manifest into the game template: the
// business service calls account, chat, mail, match and session, each hosted as its own
// process. It is opt-in (`roost project new … -template game`).
func applyGameTemplate(m *Manifest, gameService string) error {
	service, ok := m.Services[gameService]
	if !ok {
		return fmt.Errorf("template game: service %q is not declared", gameService)
	}
	for _, name := range frameworkServiceNames() {
		if existing, taken := m.Services[name]; taken && strings.TrimSpace(existing.Framework) == "" {
			return fmt.Errorf("template game: service name %q is taken by a business service", name)
		}
		m.Services[name] = ServiceSpec{Framework: name}
		if !contains(service.Uses, name) {
			service.Uses = append(service.Uses, name)
		}
	}
	sort.Strings(service.Uses)
	// Player and World are Nest entities, so the game process runs the Nest
	// runtime (which brings the dataengine, Mongo and NATS).
	if resolved, err := resolveMods(service.Mods); err == nil && !contains(resolved, "nest") {
		service.Mods = append(service.Mods, "nest")
	}
	m.Services[gameService] = service
	return nil
}

// scaffoldGameTemplate adds what the game template owns beyond the manifest:
// the Player and World entities with their lifecycles, the World singleton
// accessor, and a game Service that ensures the World exists before serving.
// Everything it writes is business-owned and written once.
func scaffoldGameTemplate(root string, m Manifest, gameService string) ([]string, error) {
	var created []string
	for _, step := range []AddOptions{
		{Kind: "entity", Name: "Player"},
		{Kind: "entity", Name: "World"},
		{Kind: "lifecycle", Name: "Player", Service: gameService},
		{Kind: "lifecycle", Name: "World", Service: gameService},
	} {
		files, err := Add(root, step)
		if err != nil {
			return created, fmt.Errorf("template game: add %s %s: %w", step.Kind, step.Name, err)
		}
		created = append(created, files...)
	}
	for rel, body := range map[string]string{
		"game/lifecycle/world_singleton.go":               renderWorldSingleton(m),
		"internal/service/" + gameService + "/service.go": renderGameTemplateService(m, gameService),
	} {
		formatted, err := format.Source([]byte(body))
		if err != nil {
			return created, fmt.Errorf("template game: format %s: %w\n%s", rel, err, body)
		}
		if err := writeAtomic(filepath.Join(root, filepath.FromSlash(rel)), formatted, 0o644); err != nil {
			return created, err
		}
		created = append(created, rel)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		return created, fmt.Errorf("template game: regenerate: %w", err)
	}
	return created, nil
}

// renderWorldSingleton is the one-World-per-server accessor. The World is an
// ordinary Nest entity whose unique id IS the server's sid, created on the
// first start of that server and loaded on every later one; "singleton" is a
// property of one game server, not of the cluster.
//
// The id has to carry the sid. A constant looks harmless while there is one
// game process, but two processes against one database then address one
// document as two entities, each with its own version counter, and the second
// commit is a `fatal projection version conflict` that takes a process down
// (GAME_DEMO_TEMPLATE §9.11).
func renderWorldSingleton(m Manifest) string {
	return fmt.Sprintf(`package lifecycle

import (
	"context"
	"fmt"

	world %q
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// WorldUniqueID is the unique id of a server's one World: its sid. There is
// exactly one World per game server, and a server is one process, so this is
// what keeps two processes sharing a database from writing one document as
// two entities.
func WorldUniqueID(registry *app.Registry) (int64, error) {
	if registry == nil {
		return 0, fmt.Errorf("world: registry is required to know which server's World this is")
	}
	sid := registry.Config().GetInt64("sid")
	if sid <= 0 {
		return 0, fmt.Errorf("world: sid is %%d; a World belongs to a server and needs its id", sid)
	}
	return sid, nil
}

// WorldID is WorldUniqueID as the full entity id the Nest senders address.
func WorldID(registry *app.Registry) (int64, error) {
	unique, err := WorldUniqueID(registry)
	if err != nil {
		return 0, err
	}
	id, err := entity.BuildEntityID(unique, world.EntityKindWorld)
	if err != nil {
		return 0, fmt.Errorf("world: build entity id: %%w", err)
	}
	return id, nil
}

// EnsureWorld returns this server's World, creating it on the first start
// and loading it on every later one. The game Service calls it from Init, so
// the World exists before any request is served.
func EnsureWorld(ctx context.Context, registry *app.Registry) (*world.World, error) {
	lifecycle, err := WorldFromRegistry(registry)
	if err != nil {
		return nil, err
	}
	unique, err := WorldUniqueID(registry)
	if err != nil {
		return nil, err
	}
	value, _, err := lifecycle.GetOrCreate(ctx, unique)
	if err != nil {
		return nil, fmt.Errorf("world: ensure singleton: %%w", err)
	}
	return value, nil
}
`, m.Project.Module+"/game/entities/world")
}

// renderGameTemplateService is the template's business Service: the plain
// scaffold plus the World it owns.
func renderGameTemplateService(m Manifest, name string) string {
	return fmt.Sprintf(`package %s

import (
	"context"
	"fmt"

	world %q
	lifecycle %q
	"github.com/tjbdwanghaibo/roost-core/app"
)

// Service is the %s server. It owns this process's World.
type Service struct {
	world *world.World
}

func New() *Service                    { return &Service{} }
func (*Service) Name() app.ServiceName { return app.ServiceName(%q) }

// Init runs after every Mod has started, so the Entity runtime is up. The
// World — this process's one singleton Entity — is created or loaded here,
// before any request is served; a game server without its World does not
// start.
func (s *Service) Init(registry *app.Registry) error {
	value, err := lifecycle.EnsureWorld(context.Background(), registry)
	if err != nil {
		return fmt.Errorf("%s: %%w", err)
	}
	s.world = value
	return nil
}

// World is this process's World. It is set in Init and never nil afterwards.
func (s *Service) World() *world.World { return s.world }

func (*Service) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (*Service) Shutdown(context.Context) error  { return nil }

var _ app.Service = (*Service)(nil)
`, safeIdent(name), m.Project.Module+"/game/entities/world", m.Project.Module+"/game/lifecycle", name, name, name)
}

// renderFrameworkServicesGuide documents the hosted framework services of this
// project: what each subcommand is, what it needs, where its collaborators
// live, and how the business service reaches it.
func renderFrameworkServicesGuide(m Manifest) string {
	var b strings.Builder
	b.WriteString("<!-- Code generated by roost-codegen. DO NOT EDIT. -->\n# 托管的框架服务\n\n")
	b.WriteString("本工程把 roost-kit/service 的通用服务作为**独立进程**托管：每个托管服务是一个子命令，进程就是该服务的 Server 加它的 owner Mod。\n")
	b.WriteString("业务 Service 通过 ClientMod 经总线调用它们，与 examples/split 的形状一致。部署产物（compose / k8s / shell）随 roost.yaml 的 services 自动覆盖每个托管服务。\n\n")
	b.WriteString("## 托管服务\n\n| 子命令 | 服务 | 配置文件 | 必填配置 | 协作者文件 |\n| --- | --- | --- | --- | --- |\n")
	for _, name := range sortedServiceNames(m) {
		spec, ok := frameworkCatalog[m.Services[name].Framework]
		if !ok {
			continue
		}
		required := spec.Package + ".key_prefix"
		if spec.Package == "account" {
			required += ", account.session_secret"
		}
		if spec.Package == "mail" {
			required += ", mail.send_ttl"
		}
		fmt.Fprintf(&b, "| `%s` | %s | configs/service/config.%s.yaml | %s | internal/service/%s/collaborators.go |\n", name, spec.Package, name, required, name)
	}
	b.WriteString("\n## 协作者：默认全部拒绝\n\n")
	b.WriteString("owner Mod 需要的协作者——身份校验、玩家 id 分配、名字规则、广播投递、频道策略、系统鉴权、匹配分组——是业务决定，生成器只生成一次、不再改写，\n")
	b.WriteString("默认实现**全部拒绝**（fail-closed）：account 不签发任何会话、chat 不允许任何发布与读取、mail 拒绝广播。这不是缺陷，是\n")
	b.WriteString("\"没有决定的信任\"不应默认存在。`roost project doctor` 的 `collaborators:<服务>` 项在文件里还含生成的 stub（\"is not configured\"）时报失败，\n")
	b.WriteString("`roost project next` 在业务链完成后也会提示。实现方式：改写对应函数，返回你自己的实现；把默认的 errors.New 分支删掉。\n\n")
	b.WriteString("## 业务 Service 如何调用\n\n")
	for _, name := range sortedServiceNames(m) {
		service := m.Services[name]
		if len(service.Uses) == 0 {
			continue
		}
		fmt.Fprintf(&b, "`%s` 进程装配了以下 ClientMod，并在 internal/service/%s/framework_clients_gen.go 生成了类型化访问器：\n\n", name, name)
		used := append([]string(nil), service.Uses...)
		sort.Strings(used)
		for _, target := range used {
			spec := frameworkCatalog[m.Services[target].Framework]
			fmt.Fprintf(&b, "- `%s(registry)` → `%s.%s`（%s.CapabilityName）\n", safeIdent(target), spec.Package, spec.Interface, spec.Package)
		}
		fmt.Fprintf(&b, "\n在 Service.Init 里取一次并保存：\n\n    svc, err := %s.Mail(registry)\n\n访问器在 capability 不存在时返回错误：说明对应进程没起、或者总线不通。\n\n", safeIdent(name))
	}
	b.WriteString("## 本地运行顺序\n\n")
	b.WriteString("    docker compose -f deploy/dev/docker-compose.yaml up -d\n    go build -o bin/app .\n")
	for _, name := range sortedServiceNames(m) {
		if m.isFrameworkService(name) {
			fmt.Fprintf(&b, "    ./bin/app %s --config configs/service/config.%s.yaml &\n", name, name)
		}
	}
	for _, name := range sortedServiceNames(m) {
		if !m.isFrameworkService(name) && len(m.Services[name].Uses) > 0 {
			fmt.Fprintf(&b, "    ./bin/app %s --config configs/service/config.%s.yaml\n", name, name)
		}
	}
	b.WriteString("\n或者一条命令：`make dev-run`（deploy/dev/run.sh）按上面的顺序启动全部服务并等每个进程的 /readyz，`make dev-stop` 反序停止，`make dev-status` 查看。\n")
	b.WriteString("每个服务在本机配置里有自己的 ops 端口（业务服务从 9100 起，托管服务接在后面，见各 config.<服务>.yaml 的 ops.addr）；生产配置统一 9100，一容器一进程。\n")
	b.WriteString("托管服务的 key_prefix 默认 roost:<工程名>:<服务>：两个部署共用一个 Redis 时必须不同，否则共享状态。\n")
	return b.String()
}
