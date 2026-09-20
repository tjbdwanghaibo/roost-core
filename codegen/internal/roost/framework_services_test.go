package roost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gameTemplateManifest(t *testing.T) Manifest {
	t.Helper()
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	if err := applyGameTemplate(&m, "game"); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

// The game template hosts every catalogued framework service as its own process
// and wires the business service to all of them.
func TestGameTemplateHostsEveryFrameworkServiceAndWiresTheGame(t *testing.T) {
	m := gameTemplateManifest(t)
	for _, name := range []string{"account", "activity", "mail", "match", "chat", "global", "platform", "rank", "session"} {
		if m.Services[name].Framework != name {
			t.Errorf("service %s is not hosted: %+v", name, m.Services[name])
		}
	}
	if got := strings.Join(m.Services["game"].Uses, ","); got != "account,activity,chat,global,mail,match,platform,rank,session" {
		t.Fatalf("game uses %q", got)
	}
	if !m.usesFrameworkServices() {
		t.Fatal("the template does not register as using roost-service")
	}
	if err := applyGameTemplate(&m, "nope"); err == nil {
		t.Fatal("a template applied to an undeclared service was accepted")
	}
}

// framework and uses are validated against the catalog and against each other.
func TestFrameworkServiceManifestValidation(t *testing.T) {
	base := func() Manifest {
		return DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	}
	unknown := base()
	unknown.Services["ledger"] = ServiceSpec{Framework: "ledger"}
	if err := unknown.Validate(); err == nil || !strings.Contains(err.Error(), "unknown framework service") {
		t.Fatalf("unknown framework accepted: %v", err)
	}
	dangling := base()
	dangling.Services["game"] = ServiceSpec{Mods: []string{"configdata"}, Uses: []string{"mail"}}
	if err := dangling.Validate(); err == nil || !strings.Contains(err.Error(), "not a framework service") {
		t.Fatalf("uses of an undeclared framework service accepted: %v", err)
	}
	hostedUses := base()
	hostedUses.Services["mail"] = ServiceSpec{Framework: "mail", Uses: []string{"chat"}}
	hostedUses.Services["chat"] = ServiceSpec{Framework: "chat"}
	if err := hostedUses.Validate(); err == nil || !strings.Contains(err.Error(), "cannot also declare uses") {
		t.Fatalf("a hosted service with uses accepted: %v", err)
	}
	legacy := base()
	legacy.Versions.Service = "v1.5.4" // written by a generator that predates the consolidation
	if err := legacy.Validate(); err != nil {
		t.Fatalf("a pre-consolidation versions.service must still load (the module is folded into kit): %v", err)
	}
}

// What the generator emits for a hosted service and for a service that calls
// it: the Server as the process, the owner Mod from the collaborators file,
// Redis and NATS assembled, the ClientMod and typed accessor on the caller,
// roost-service in go.mod and retained by frameworkdeps, and the service's
// configuration keys in its config file.
func TestGameTemplateRendersHostingAndClientWiring(t *testing.T) {
	m := gameTemplateManifest(t)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := string(plan["internal/bootstrap/generated.go"].Body)
	for _, want := range []string{
		`a.RegisterServer(app.ServiceName(svcmail.ServiceType), svcmail.NewServer()`,
		`svcmail.NewMod(serviceMail.Broadcast(), serviceMail.Metrics())`,
		`svcaccount.NewMod(serviceAccount.Verifier(), serviceAccount.Allocator(), serviceAccount.NameRules(), serviceAccount.Metrics())`,
		`svcchat.NewMod(serviceChat.Policy(), serviceChat.Bodies(), serviceChat.System(), serviceChat.Rules(), serviceChat.Metrics())`,
		`svcmatch.NewMod(serviceMatch.Metrics())`,
		`kitredis.NewRedisMod()`, `kitnats.NewNatsMod(nil)`,
		`a.RegisterServer(app.ServiceName("game"), serviceGame.New()`,
		`svcaccount.NewClientMod()`, `svcmail.NewClientMod()`, `svcmatch.NewClientMod()`, `svcchat.NewClientMod()`,
	} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap missing %q:\n%s", want, bootstrap)
		}
	}
	for _, path := range []string{
		"internal/service/mail/collaborators.go", "internal/service/account/collaborators.go",
		"internal/service/match/collaborators.go", "internal/service/chat/collaborators.go",
	} {
		file, ok := plan[path]
		if !ok || file.Owned {
			t.Errorf("%s must exist and be business-owned (owned=%v present=%v)", path, file.Owned, ok)
		}
	}
	if _, ok := plan["internal/service/mail/service.go"]; ok {
		t.Error("a hosted service must not get a business service.go")
	}
	clients, ok := plan["internal/service/game/framework_clients_gen.go"]
	if !ok || !clients.Owned {
		t.Fatalf("typed accessors missing or not generator-owned: present=%v", ok)
	}
	for _, want := range []string{"func Mail(r *app.Registry) (svcmail.Mail, error)", "func Account(r *app.Registry) (svcaccount.Accounts, error)", "svcchat.Messaging", "svcmatch.Matchmaker"} {
		if !strings.Contains(string(clients.Body), want) {
			t.Errorf("accessors missing %q", want)
		}
	}
	if gomod := string(plan["go.mod"].Body); strings.Contains(gomod, "roost-service") {
		t.Errorf("go.mod must not require the folded-in roost-service module:\n%s", gomod)
	}
	if deps := string(plan["internal/frameworkdeps/generated.go"].Body); !strings.Contains(deps, "roost-kit/service/servicemetrics") {
		t.Errorf("frameworkdeps does not retain the kit services:\n%s", deps)
	}
	mailConfig := string(plan["configs/service/config.mail.yaml"].Body)
	for _, want := range []string{"mail:\n  key_prefix: roost:planet:mail", "send_ttl: 720h", "redis:\n", "nats:\n"} {
		if !strings.Contains(mailConfig, want) {
			t.Errorf("mail config missing %q:\n%s", want, mailConfig)
		}
	}
	if accountConfig := string(plan["configs/service/config.account.prod.example.yaml"].Body); !strings.Contains(accountConfig, "session_secret: CHANGE_ME") {
		t.Errorf("account production example does not demand a secret:\n%s", accountConfig)
	}
	if gameConfig := string(plan["configs/service/config.game.yaml"].Body); !strings.Contains(gameConfig, "nats:\n") {
		t.Errorf("a service that calls framework services needs the bus, so nats config:\n%s", gameConfig)
	}
}

// A project that neither hosts nor calls a framework service carries no
// roost-service dependency at all.
func TestAProjectWithoutFrameworkServicesDoesNotDependOnRoostService(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"go.mod", "internal/frameworkdeps/generated.go", "internal/bootstrap/generated.go"} {
		if strings.Contains(string(plan[path].Body), "roost-service") {
			t.Errorf("%s mentions roost-service without any framework service", path)
		}
	}
}

// The game template scaffolds Player and World with their lifecycles, the
// World singleton accessor, and a game Service that ensures the World in
// Init — and the two lifecycle files must coexist in one package, which they
// did not while both declared FromRegistry.
func TestGameTemplateScaffoldsWorldAndPlayer(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	result, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"}, Template: "game"})
	if err != nil {
		t.Fatalf("new project: %v (created %v)", err, result.Created)
	}
	for _, rel := range []string{
		"game/entities/player/entity.go", "game/entities/world/entity.go",
		"game/lifecycle/player.go", "game/lifecycle/world.go", "game/lifecycle/world_singleton.go",
		"internal/service/game/service.go", "internal/service/game/framework_clients_gen.go",
		"internal/service/mail/collaborators.go",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s missing: %v", rel, err)
		}
	}
	service, _ := os.ReadFile(filepath.Join(root, "internal", "service", "game", "service.go"))
	if !strings.Contains(string(service), "lifecycle.EnsureWorld(") {
		t.Errorf("the template game Service does not ensure its World:\n%s", service)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := resolveMods(m.Services["game"].Mods); !contains(resolved, "nest") {
		t.Errorf("template game service lacks the nest runtime: %v", m.Services["game"].Mods)
	}
	// Both lifecycles in one package: no redeclared top-level identifier.
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join(root, "game", "lifecycle"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, pkg := range pkgs {
		for file, f := range pkg.Files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil {
					continue
				}
				if prev, dup := seen[fn.Name.Name]; dup {
					t.Errorf("%s is declared in both %s and %s", fn.Name.Name, prev, file)
				}
				seen[fn.Name.Name] = file
			}
		}
	}
	for _, want := range []string{"PlayerFromRegistry", "WorldFromRegistry", "EnsureWorld"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("lifecycle package lacks %s", want)
		}
	}
}

// doctor reports each hosted service whose collaborators are still the
// generated fail-closed stubs, and clears the item once they are replaced.
func TestDoctorReportsUnimplementedCollaborators(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}, Template: "game"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	failing := map[string]bool{}
	for _, item := range checkFrameworkCollaborators(root, m) {
		if item.Status == StatusFail {
			failing[item.Name] = true
		}
	}
	for _, name := range []string{"collaborators:account", "collaborators:chat"} {
		if !failing[name] {
			t.Errorf("%s is not reported although its stubs are untouched", name)
		}
	}
	// mail's only stub is Broadcast() returning nil, which is a legitimate
	// configuration (broadcasts refused), so mail carries no marker.
	if failing["collaborators:mail"] {
		t.Error("mail is reported although a nil Broadcast is a valid, fail-closed choice")
	}
	path := filepath.Join(root, "internal", "service", "chat", "collaborators.go")
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), "is not configured", "is refused by project policy")), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, item := range checkFrameworkCollaborators(root, m) {
		if item.Name == "collaborators:chat" && item.Status != StatusOK {
			t.Errorf("chat still reported after its stubs were replaced: %s", item.Detail)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "SERVICES.zh-CN.md")); err != nil {
		t.Errorf("template project lacks docs/SERVICES.zh-CN.md: %v", err)
	}
	plain := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	plan, err := renderProject(plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plan["docs/SERVICES.zh-CN.md"]; ok {
		t.Error("a project without framework services gets a SERVICES guide")
	}
}
