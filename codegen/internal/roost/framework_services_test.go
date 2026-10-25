package roost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
		`a.RegisterServer(app.ServiceName(domainmail.ServiceType), svcmail.NewServer()`,
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
	if deps := string(plan["internal/frameworkdeps/generated.go"].Body); !strings.Contains(deps, "roost-core/infra/observe/servicemetrics") {
		t.Errorf("frameworkdeps does not retain service metrics:\n%s", deps)
	}
	mailConfig := string(plan["configs/service/config.mail.yaml"].Body)
	for _, want := range []string{"mail:\n", "  key_prefix: roost:planet:mail\n", "send_ttl: 720h", "redis:\n", "nats:\n"} {
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
	t.Parallel()
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
	t.Parallel()
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

// 生成的客户端按亲和键路由的框架服务两侧都需要 etcd 发现（ClientMod 没有它拒绝启动，owner 要注册 sid 才能被找到）：
// catalog 的 Affinity 标记决定是否加 etcd Mod，必须与生成的 transport 一致——有方法按键路由时才声明 AffinityMethods。
func TestFrameworkCatalogAffinityMatchesTheGeneratedClients(t *testing.T) {
	repo := filepath.Join("..", "..", "..")
	for name, spec := range frameworkCatalog {
		var transports []string
		for _, dir := range []string{filepath.Join(repo, "wiring", spec.ImportPath()), filepath.Join(repo, "service", spec.Package)} {
			matches, _ := filepath.Glob(filepath.Join(dir, "*_rpc_gen.go"))
			transports = append(transports, matches...)
		}
		if len(transports) == 0 {
			t.Fatalf("%s: no generated transport found", name)
		}
		declares := false
		for _, path := range transports {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			declares = declares || strings.Contains(string(raw), "\nconst AffinityMethods = ")
		}
		if declares != spec.Affinity {
			t.Errorf("%s: catalog Affinity=%v but the generated client declares AffinityMethods=%v", name, spec.Affinity, declares)
		}
	}

	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata", "nest"}, nil)
	m.Services["match"] = ServiceSpec{Framework: "match"}
	m.Services["mail"] = ServiceSpec{Framework: "mail"}
	game := m.Services["game"]
	game.Uses = []string{"mail"}
	m.Services["game"] = game
	if contains(effectiveServiceMods(m, "game"), "etcd") || contains(effectiveServiceMods(m, "mail"), "etcd") {
		t.Fatal("mail routes nothing by key; neither its owner nor its caller needs etcd")
	}
	game.Uses = []string{"mail", "match"}
	m.Services["game"] = game
	if !contains(effectiveServiceMods(m, "game"), "etcd") {
		t.Fatalf("game calls match (affinity) but gets no etcd Mod: %v", effectiveServiceMods(m, "game"))
	}
	if !contains(effectiveServiceMods(m, "match"), "etcd") {
		t.Fatalf("the match owner does not register in etcd: %v", effectiveServiceMods(m, "match"))
	}
}

// A hosted framework service sometimes has an OPTIONAL collaborator that is
// not a NewMod parameter — platform's pending-order index is the first
// (kit `Mod.WithPendingOrders`). The generated wiring has to call it, or the
// project's collaborators file can define it and nothing will ever read it.
//
// The second promise here is the configuration block: platform's Mod refuses
// at Init when `session_secret` or `payment_secret` is empty, so a generated
// starter config that omits them produces a process that cannot start. The
// account block already emits `session_secret: CHANGE_ME` for exactly this
// reason.

func TestHostedServiceChainsItsOptionalCollaborators(t *testing.T) {
	generated := renderBootstrap(gameTemplateManifest(t))
	const want = "svcplatform.NewMod(servicePlatform.Verify(), servicePlatform.Players(), servicePlatform.Deliver(), servicePlatform.Metrics()).WithPendingOrders(servicePlatform.Pending())"
	if !strings.Contains(generated, want) {
		t.Errorf("the platform Mod is wired without its pending-order index:\n%s", generated)
	}
}

func TestPlatformConfigCarriesTheSecretsItsModRequires(t *testing.T) {
	block := frameworkConfigSection("platform", "demo")
	for _, key := range []string{"session_secret:", "payment_secret:"} {
		if !strings.Contains(block, key) {
			t.Errorf("platform config block omits %s, so the process refuses to start:\n%s", key, block)
		}
	}
}

// The default collaborators file has to define everything the wiring names,
// including the optional ones, or the project does not compile.
func TestDefaultCollaboratorsDefineEveryWiredName(t *testing.T) {
	body := renderFrameworkCollaborators(gameTemplateManifest(t), "platform")
	for _, fn := range []string{"func Verify()", "func Players()", "func Deliver()", "func Pending()", "func Metrics()"} {
		if !strings.Contains(body, fn) {
			t.Errorf("collaborators file is missing %s:\n%s", fn, body)
		}
	}
}

// A kit service whose Go package is not at the top of service/ — activity
// lives at service/global/activity — must still be hostable. The catalog's
// Package is both an identifier (svcactivity) and an import suffix, and those
// are not the same string here; a catalog that conflates them generates
// `svcglobal/activity`, which is not an identifier.
func TestAServiceNestedUnderAnotherIsHostable(t *testing.T) {
	m := gameTemplateManifest(t)
	for _, name := range []string{"global", "activity"} {
		if m.Services[name].Framework != name {
			t.Fatalf("the game template does not host %s: %+v", name, m.Services[name])
		}
	}
	generated := renderBootstrap(m)
	for _, want := range []string{
		`svcactivity "github.com/tjbdwanghaibo/roost-core/wiring/activity"`,
		"svcactivity.NewMod(serviceActivity.Metrics())",
		"svcglobal.NewMod(serviceGlobal.Metrics())",
	} {
		if !strings.Contains(generated, want) {
			t.Errorf("bootstrap is missing %q:\n%s", want, generated)
		}
	}
}

// The activity Mod refuses an unset reservation_ttl at Init (it must exceed
// the caller's retry horizon, which the service cannot pick), and since
// 2026-10-06 an unset groups_file (nothing to check a window's expected set
// against), so a starter config that omits either is a process that cannot
// start.
func TestActivityConfigCarriesTheTTLItsModRequires(t *testing.T) {
	block := frameworkConfigSection("activity", "demo")
	for _, want := range []string{"reservation_ttl:", "groups_file: " + activityGroupsFile + "\n"} {
		if !strings.Contains(block, want) {
			t.Errorf("activity config block omits %q:\n%s", want, block)
		}
	}
}

// 每个托管服务的 collaborators 文件里，每一个 import 都被正文里的选择表达式引用（注释里出现的 `match.Grouping` 不算）：
// 无条件 import 服务包会让生成文件 "imported and not used"、整个工程编译不过。
func TestFrameworkCollaboratorsImportOnlyWhatTheyUse(t *testing.T) {
	for framework := range frameworkCatalog {
		m := DefaultManifest("planet", "example.com/planet", []string{framework}, nil, nil)
		m.Services[framework] = ServiceSpec{Framework: framework}
		src := renderFrameworkCollaborators(m, framework)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, framework+"/collaborators.go", src, 0)
		if err != nil {
			t.Fatalf("%s collaborators do not parse: %v\n%s", framework, err, src)
		}
		used := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok {
					used[ident.Name] = true
				}
			}
			return true
		})
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			name := path.Base(importPath)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if !used[name] {
				t.Errorf("%s collaborators import %q but never use it:\n%s", framework, importPath, src)
			}
		}
	}
}

// C6（维护者决定，docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md）：生成工程的每个托管服务默认把
// servicemetrics 事件写进进程的 metrics 注册表（ops /metrics 导出），而不是 Metrics() 返回 nil、
// 事件无处落地（N06 观察 1、N12 O1）。承诺：
//   - 生成器写的 collaborators.go 与 game-demo 自带的 collaborators.go，Metrics() 都返回
//     servicemetrics.NewMetricsReporter("<服务名>")；
//   - 注释说明怎么关：返回 nil，或配置 service_metrics.enabled: false。

var metricsCollaborator = regexp.MustCompile(`func Metrics\(\) servicemetrics\.Reporter \{\s*return ([^\n}]*)\}`)

func TestGeneratedServicesReportIntoTheMetricsRegistryByDefault(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range sortedServiceNames(m) {
		if !m.isFrameworkService(name) {
			continue
		}
		rel := "internal/service/" + name + "/collaborators.go"
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		match := metricsCollaborator.FindStringSubmatch(string(raw))
		if match == nil {
			t.Fatalf("%s has no Metrics() collaborator", rel)
		}
		want := `servicemetrics.NewMetricsReporter("` + name + `")`
		if got := strings.TrimSpace(match[1]); got != want {
			t.Fatalf("%s: Metrics() returns %s, want %s: a nil reporter leaves the service's events with nowhere to go", rel, got, want)
		}
		if !strings.Contains(string(raw), "service_metrics.enabled") {
			t.Fatalf("%s does not say how to turn the default reporter off", rel)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("game-demo hosts no framework service")
	}
	// A project the generator writes from scratch (not the demo's own files).
	plain := Manifest{Project: ProjectSpec{Name: "plain"}, Services: map[string]ServiceSpec{"rank": {Framework: "rank"}}}
	match := metricsCollaborator.FindStringSubmatch(renderFrameworkCollaborators(plain, "rank"))
	if match == nil || strings.TrimSpace(match[1]) != `servicemetrics.NewMetricsReporter("rank")` {
		t.Fatalf("the generated rank collaborators return %v, want the default reporter", match)
	}
}

// servicePackages 读出 internal/service/<name>/ 下每个非测试 .go 文件的 package 子句。
func servicePackages(t *testing.T, root, service string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "internal", "service", service, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(path)] = file.Name.Name
	}
	if len(out) == 0 {
		t.Fatalf("internal/service/%s has no Go files", service)
	}
	return out
}

// D8（维护者 2026-10-16 决定 C）：新工程的服务包名按 Go 惯例小写；已有工程按目录里现有的包名生成，
// 不改写业务文件（service.go、managers.go、collaborators.go 归业务），也不能让生成器持有的文件
// （framework_clients_gen.go）换成另一个包名——同一目录两个包名，project sync 之后编译失败。
func TestServicePackageNamesAreLowercaseAndFollowAnExistingProject(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"}, Template: "game"}); err != nil {
		t.Fatal(err)
	}
	for service, want := range map[string]string{"game": "game", "account": "account", "mail": "mail"} {
		for file, got := range servicePackages(t, target, service) {
			if got != want {
				t.Errorf("new project: internal/service/%s/%s is package %s, want %s", service, file, got, want)
			}
		}
	}

	// 模拟本次改动之前生成的工程：目录里全是大写包名。
	upper := map[string]string{"game": "Game", "account": "Account"}
	business := map[string][]byte{}
	for service, pkg := range upper {
		paths, _ := filepath.Glob(filepath.Join(target, "internal", "service", service, "*.go"))
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.Replace(string(raw), "package "+service+"\n", "package "+pkg+"\n", 1))
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(raw), generatedHeader) {
				business[path] = raw
			}
		}
	}
	// 生成器持有的文件丢了也要按目录现有包名重建。
	if err := os.Remove(filepath.Join(target, "internal", "service", "game", "framework_clients_gen.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncProject(target); err != nil {
		t.Fatalf("sync an existing upper-case project: %v", err)
	}
	for service, want := range upper {
		for file, got := range servicePackages(t, target, service) {
			if got != want {
				t.Errorf("existing project after sync: internal/service/%s/%s is package %s, want the directory's %s", service, file, got, want)
			}
		}
	}
	for path, before := range business {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Errorf("sync rewrote business file %s", path)
		}
	}
}

// 生成的 mail Broadcast 骨架必须按 service/mail 的契约说明广播的受众：AudienceBroadcast 的范围是一个服 / 公会 /
// 全体（含离线玩家与之后才登录的玩家），投递策略由部署选择（mail.Deliverer）。注释写成“投给每个在线玩家”会把业务
// 引向漏发离线玩家的实现。
func TestMailBroadcastSkeletonDescribesTheWholeAudience(t *testing.T) {
	plan, err := renderProject(gameTemplateManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	file, ok := plan["internal/service/mail/collaborators.go"]
	if !ok {
		t.Fatal("internal/service/mail/collaborators.go missing")
	}
	body := string(file.Body)
	if strings.Contains(body, "online player") {
		t.Errorf("mail Broadcast skeleton describes the broadcast audience as online players only:\n%s", body)
	}
	// 全服邮件：骨架缺省仍为 nil，注释给出内置“存一份 + 拉取合并”的一行接法与登录钩子。
	if !strings.Contains(body, "func Broadcast() mail.Deliverer { return nil }") {
		t.Errorf("mail Broadcast skeleton no longer defaults to nil (refuse broadcasts):\n%s", body)
	}
	for _, want := range []string{"offline", "mail.Deliverer", "mail.NewLazyBroadcast(mail.LazyBroadcastConfig{})", "PullBroadcasts", "RegisteredAtUnix"} {
		if !strings.Contains(body, want) {
			t.Errorf("mail Broadcast skeleton does not mention %q:\n%s", want, body)
		}
	}
}
